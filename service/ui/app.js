(function () {
  "use strict";
  var $ = function (id) { return document.getElementById(id); };
  var timer = null, deploymentId = null, baseVersion = null;

  function api(method, path, body) {
    var headers = { "Accept": "application/json" };
    var tok = $("token").value.trim();
    if (tok) headers["Authorization"] = "Bearer " + tok;
    if (body) headers["Content-Type"] = "application/json";
    return fetch(path, { method: method, headers: headers, body: body ? JSON.stringify(body) : undefined })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; }); });
  }

  function drawFleet(f) {
    var grid = $("grid");
    if (grid.children.length !== f.hosts.length) {
      grid.textContent = "";
      f.hosts.forEach(function () { var c = document.createElement("div"); c.className = "cell"; grid.appendChild(c); });
    }
    f.hosts.forEach(function (h, i) {
      var cls = "cell";
      if (h.state === "serving") { if (baseVersion && h.version !== baseVersion) cls += " new"; }
      else if (h.state === "draining") cls += " draining";
      else if (h.state === "updating") cls += " updating";
      else cls += " out";
      var c = grid.children[i];
      if (c.className !== cls) c.className = cls;
      c.title = h.id + " " + h.version + " " + h.state;
    });
    var parts = Object.keys(f.versions).sort().map(function (k) { return k + ": " + f.versions[k]; });
    $("versions").textContent = f.total + " hosts (" + parts.join(", ") + ")";
  }

  function drawLog(events) {
    var log = $("log");
    log.textContent = "";
    events.slice(-40).forEach(function (e) {
      var li = document.createElement("li");
      var t = new Date(e.at);
      var stamp = t.toTimeString().slice(0, 8) + "." + String(t.getMilliseconds()).padStart(3, "0");
      li.textContent = stamp + "  " + e.type + (e.target ? "  " + e.target : "") + (e.detail ? "  " + e.detail : "");
      if (/failed|fail$|rollback_start/.test(e.type)) li.className = "bad";
      if (/done|pass|rolled_back$/.test(e.type)) li.className = "good";
      log.appendChild(li);
    });
    log.scrollTop = log.scrollHeight;
  }

  function drawRun(v) {
    var p = v.progress;
    var label = v.running ? p.phase || "running" : (v.report ? v.report.status : p.status);
    $("status").textContent = label.replace(/_/g, " ");
    $("done").textContent = p.done + " / " + p.total;
    $("out").textContent = p.out;
    $("peak").textContent = p.peakOut;
    $("minin").textContent = v.minInRotation > 0 ? v.minInRotation + " / " + v.fleetSize : "-";
    var pct = p.total ? Math.min(100, Math.round(100 * p.done / p.total)) : 0;
    $("barfill").style.width = pct + "%";
    $("bar").setAttribute("aria-valuenow", String(pct));
    $("reason").textContent = v.report && v.report.reason ? v.report.reason : "";
    drawLog(v.events || []);
    $("go").disabled = v.running;
    $("cancel").disabled = !v.running;
  }

  function poll() {
    Promise.all([api("GET", "/api/fleet"), deploymentId ? api("GET", "/api/deployments/" + deploymentId) : Promise.resolve(null)])
      .then(function (rs) {
        drawFleet(rs[0].body);
        if (rs[1] && rs[1].ok) {
          drawRun(rs[1].body);
          if (!rs[1].body.running) { clearInterval(timer); timer = null; }
        }
      })
      .catch(function () { $("error").textContent = "lost contact with the server"; });
  }

  $("deploy").addEventListener("submit", function (ev) {
    ev.preventDefault();
    $("error").textContent = "";
    api("GET", "/api/fleet").then(function (f) {
      // The most common build in the fleet before the deploy is what "old" means on the grid.
      var best = null, n = 0;
      Object.keys(f.body.versions).forEach(function (k) { if (f.body.versions[k] > n) { n = f.body.versions[k]; best = k; } });
      baseVersion = best;
      var body = {
        version: $("version").value.trim(),
        batchPercent: Number($("batch").value),
        canary: Number($("canary").value),
        failHost: $("failHost").checked ? "host-0042" : "",
        badCanary: $("badCanary").checked
      };
      return api("POST", "/api/deployments", body);
    }).then(function (r) {
      if (!r.ok) { $("error").textContent = r.body.error || ("request failed (" + r.status + ")"); return; }
      deploymentId = r.body.id;
      $("go").disabled = true; $("cancel").disabled = false;
      if (timer) clearInterval(timer);
      timer = setInterval(poll, 200);
      poll();
    });
  });

  $("cancel").addEventListener("click", function () {
    if (deploymentId) api("POST", "/api/deployments/" + deploymentId + "/cancel");
  });

  api("GET", "/api/fleet").then(function (f) { drawFleet(f.body); });
})();
