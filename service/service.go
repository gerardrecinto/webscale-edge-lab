// Package service is the push-button front end: a small REST API and web page
// over the deploy engine. It drives a simulated fleet, so anyone can run it on a
// laptop and watch a rollout, a canary and a rollback happen.
//
//	GET  /                          the page
//	GET  /api/fleet                 every host's build and state
//	POST /api/deployments           start a deployment (202), 409 if one is running
//	GET  /api/deployments           recent deployments
//	GET  /api/deployments/{id}      progress, and the report when it has finished
//	POST /api/deployments/{id}/cancel
//	GET  /metrics, /healthz
//
// When Token is set, every POST needs "Authorization: Bearer <token>".
package service

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gerardrecinto/webscale-edge-lab/backend"
	"github.com/gerardrecinto/webscale-edge-lab/deploy"
)

//go:embed ui
var uiFiles embed.FS

var versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

type Service struct {
	Fleet *deploy.Sim
	Token string
	// Emit receives every deployment event, for example to forward to Splunk.
	Emit func(deploymentID string, e deploy.Event)

	mu      sync.Mutex
	current *run
	history []*run
	nextID  int
	counts  map[string]int
	peak    int
}

type run struct {
	ID      string
	Version string
	Started time.Time
	Engine  *deploy.Engine
	cancel  context.CancelFunc
	done    chan struct{}

	mu     sync.Mutex
	events []deploy.Event
	report *deploy.Report
	minIn  int
}

func New(f *deploy.Sim, token string) *Service {
	return &Service{Fleet: f, Token: token, counts: map[string]int{}}
}

type Request struct {
	Version      string  `json:"version"`
	BatchPercent float64 `json:"batchPercent"`
	Canary       int     `json:"canary"`
	// For demonstrations: break something on purpose.
	FailHost  string `json:"failHost,omitempty"`  // this host's readiness check fails on the new build
	BadCanary bool   `json:"badCanary,omitempty"` // the canary looks bad to the observer
}

func (r *Request) validate(hosts int) error {
	if !versionRE.MatchString(r.Version) {
		return fmt.Errorf("version must be 1 to 32 letters, digits, dots, dashes or underscores")
	}
	if r.BatchPercent <= 0 || r.BatchPercent > 50 {
		return fmt.Errorf("batchPercent must be above 0 and at most 50")
	}
	if r.Canary < 0 || r.Canary > hosts/2 {
		return fmt.Errorf("canary must be between 0 and half the fleet (%d)", hosts/2)
	}
	return nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/fleet", s.fleet)
	mux.HandleFunc("GET /api/deployments", s.list)
	mux.HandleFunc("POST /api/deployments", s.auth(s.start))
	mux.HandleFunc("GET /api/deployments/{id}", s.get)
	mux.HandleFunc("POST /api/deployments/{id}/cancel", s.auth(s.cancel))
	return secure(mux)
}

// secure adds the response headers the OWASP Secure Headers Project recommends,
// with a CSP that only allows the page's own script and style files.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.SecurityHeaders(w.Header())
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pushbutton"`)
				writeJSON(w, 401, map[string]string{"error": "a bearer token is required"})
				return
			}
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Service) fleet(w http.ResponseWriter, r *http.Request) {
	states := s.Fleet.States()
	writeJSON(w, 200, map[string]any{"total": len(states), "versions": s.Fleet.Versions(), "hosts": states})
}

func (s *Service) start(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request body: " + err.Error()})
		return
	}
	targets := s.Fleet.Targets()
	if err := req.validate(len(targets)); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.FailHost != "" && s.Fleet.Host(req.FailHost) == nil {
		writeJSON(w, 400, map[string]string{"error": "unknown host " + req.FailHost})
		return
	}
	s.mu.Lock()
	if s.current != nil {
		select {
		case <-s.current.done:
		default:
			s.mu.Unlock()
			writeJSON(w, 409, map[string]string{"error": "deployment " + s.current.ID + " is still running"})
			return
		}
	}
	s.nextID++
	id := fmt.Sprintf("d%03d", s.nextID)
	ctx, cancel := context.WithCancel(context.Background())
	ru := &run{ID: id, Version: req.Version, Started: time.Now(), Engine: &deploy.Engine{}, cancel: cancel, done: make(chan struct{})}
	ru.Engine.Emit = func(e deploy.Event) {
		ru.mu.Lock()
		ru.events = append(ru.events, e)
		ru.mu.Unlock()
		if s.Emit != nil {
			s.Emit(id, e)
		}
	}
	s.current = ru
	s.history = append(s.history, ru)
	s.mu.Unlock()

	if req.FailHost != "" {
		s.Fleet.Host(req.FailHost).FailAt(req.Version, "verify", "readiness probe returned 503 (injected)")
	}
	plan := deploy.Plan{
		Version:        req.Version,
		MaxUnavailable: deploy.PercentBatch(len(targets), req.BatchPercent),
		Canary:         req.Canary,
		CanaryWindow:   400 * time.Millisecond,
		StepTimeout:    30 * time.Second,
		Judge:          deploy.Judge{MinRequests: 100, MaxErrorRate: 0.02, MaxErrorGap: 0.01, MaxP95Ratio: 1.5},
		Observe: func(context.Context, time.Time) (deploy.Window, deploy.Window, error) {
			c := deploy.Window{Requests: 600, Errors: 2, P95: 22 * time.Millisecond}
			if req.BadCanary {
				c.Errors = 150
			}
			return c, deploy.Window{Requests: 6000, Errors: 5, P95: 20 * time.Millisecond}, nil
		},
	}
	s.Fleet.ResetMin()
	go func() {
		rep := ru.Engine.Run(ctx, targets, plan)
		ru.mu.Lock()
		ru.report = &rep
		ru.minIn = s.Fleet.MinInRotation()
		ru.mu.Unlock()
		s.mu.Lock()
		s.counts[rep.Status]++
		if rep.PeakOut > s.peak {
			s.peak = rep.PeakOut
		}
		s.mu.Unlock()
		close(ru.done)
	}()
	writeJSON(w, 202, map[string]string{"id": id})
}

type view struct {
	ID       string          `json:"id"`
	Version  string          `json:"version"`
	Started  time.Time       `json:"started"`
	Running  bool            `json:"running"`
	Progress deploy.Progress `json:"progress"`
	MinInRot int             `json:"minInRotation"`
	Fleet    int             `json:"fleetSize"`
	Events   []deploy.Event  `json:"events"`
	Report   *deploy.Report  `json:"report,omitempty"`
}

func (s *Service) view(ru *run, withEvents bool) view {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	v := view{ID: ru.ID, Version: ru.Version, Started: ru.Started, Progress: ru.Engine.Snapshot(), Report: ru.report, Fleet: len(s.Fleet.Targets())}
	v.Running = ru.report == nil
	v.MinInRot = ru.minIn
	if v.Running {
		v.MinInRot = s.Fleet.MinInRotation()
	}
	if withEvents {
		n := len(ru.events)
		from := 0
		if n > 60 {
			from = n - 60
		}
		v.Events = append([]deploy.Event(nil), ru.events[from:]...)
	}
	if v.Report != nil { // the report carries every event already
		rep := *v.Report
		rep.Events = nil
		v.Report = &rep
	}
	return v
}

func (s *Service) find(id string) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "current" {
		return s.current
	}
	for _, r := range s.history {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	ru := s.find(r.PathValue("id"))
	if ru == nil {
		writeJSON(w, 404, map[string]string{"error": "no such deployment"})
		return
	}
	writeJSON(w, 200, s.view(ru, true))
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hist := append([]*run(nil), s.history...)
	s.mu.Unlock()
	sort.Slice(hist, func(i, j int) bool { return hist[i].Started.After(hist[j].Started) })
	if len(hist) > 20 {
		hist = hist[:20]
	}
	out := make([]view, 0, len(hist))
	for _, ru := range hist {
		out = append(out, s.view(ru, false))
	}
	writeJSON(w, 200, out)
}

func (s *Service) cancel(w http.ResponseWriter, r *http.Request) {
	ru := s.find(r.PathValue("id"))
	if ru == nil {
		writeJSON(w, 404, map[string]string{"error": "no such deployment"})
		return
	}
	ru.cancel()
	writeJSON(w, 202, map[string]string{"id": ru.ID, "status": "cancelling"})
}

func (s *Service) metrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP pushbutton_deployments_total Finished deployments by outcome.")
	fmt.Fprintln(w, "# TYPE pushbutton_deployments_total counter")
	for _, st := range []string{deploy.StatusSucceeded, deploy.StatusRolledBack, deploy.StatusRollbackFailed, deploy.StatusAborted} {
		fmt.Fprintf(w, "pushbutton_deployments_total{status=%q} %d\n", st, s.counts[st])
	}
	fmt.Fprintln(w, "# HELP pushbutton_peak_unavailable The most targets out of rotation at once in any deployment.")
	fmt.Fprintln(w, "# TYPE pushbutton_peak_unavailable gauge")
	fmt.Fprintf(w, "pushbutton_peak_unavailable %d\n", s.peak)
	fmt.Fprintln(w, "# HELP pushbutton_fleet_targets Targets by running build.")
	fmt.Fprintln(w, "# TYPE pushbutton_fleet_targets gauge")
	vs := s.Fleet.Versions()
	keys := make([]string, 0, len(vs))
	for k := range vs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "pushbutton_fleet_targets{version=%q} %d\n", k, vs[k])
	}
}
