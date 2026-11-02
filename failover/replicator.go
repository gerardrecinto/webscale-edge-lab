package failover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Replicate pulls entries from src over HTTP and applies them to dst, only
// once they are at least lag old. Because it reads over HTTP, replication
// stops the moment src is killed: nothing newer than that reaches dst.
func Replicate(ctx context.Context, src, dst *Region, lag time.Duration) {
	cl := &http.Client{Timeout: 500 * time.Millisecond}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		from := dst.State().Seq + 1
		u := fmt.Sprintf("%s/log?from=%d&older=%s", src.URL(), from, lag)
		resp, err := cl.Get(u)
		if err != nil {
			continue
		}
		var es []Entry
		json.NewDecoder(resp.Body).Decode(&es)
		resp.Body.Close()
		for _, e := range es {
			if !dst.Apply(e) {
				break
			}
		}
	}
}
