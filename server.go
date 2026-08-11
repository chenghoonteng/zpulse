package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
)

// funcMap provides the small template helpers used by the dashboard.
var funcMap = template.FuncMap{
	"healthClass": healthClass,
}

// dashboard is rendered on every request. The page uses a plain meta-refresh so
// the whole thing stays server-side — no JavaScript, no API to keep in sync.
var dashboard = template.Must(template.New("dashboard").Funcs(funcMap).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{.Refresh}}">
<title>zpulse</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 0; background: #14161a; color: #e6e6e6; }
  header { padding: 1rem 1.5rem; border-bottom: 1px solid #2a2e35; }
  header h1 { margin: 0; font-size: 1.25rem; }
  header .sub { color: #8b93a1; font-size: .8rem; }
  main { padding: 1.5rem; max-width: 1000px; }
  section { margin-bottom: 2rem; }
  h2 { font-size: 1rem; text-transform: uppercase; letter-spacing: .05em; color: #8b93a1; border-bottom: 1px solid #2a2e35; padding-bottom: .4rem; }
  table { border-collapse: collapse; width: 100%; font-variant-numeric: tabular-nums; }
  th, td { text-align: left; padding: .45rem .75rem; border-bottom: 1px solid #22262d; }
  th { color: #8b93a1; font-weight: 600; font-size: .8rem; }
  .online { color: #4ade80; }
  .degraded, .faulted, .unavail, .offline { color: #f87171; }
  .empty { color: #8b93a1; font-style: italic; }
  .err { background: #3a1e1e; color: #f8a5a5; padding: .75rem 1rem; border-radius: 6px; margin-bottom: 1.5rem; }
  .bar { background: #22262d; border-radius: 4px; height: 10px; width: 140px; overflow: hidden; display: inline-block; vertical-align: middle; }
  .bar > span { display: block; height: 100%; background: #4ade80; }
</style>
</head>
<body>
<header>
  <h1>zpulse</h1>
  <div class="sub">ZFS pool dashboard &middot; refreshing every {{.Refresh}}s</div>
</header>
<main>
  {{if .Err}}<div class="err">{{.Err}}</div>{{end}}

  <section>
    <h2>Pools</h2>
    {{if .Pools}}
    <table>
      <thead><tr><th>Name</th><th>Health</th><th>Used</th><th>Size</th><th>Cap</th><th>Frag</th></tr></thead>
      <tbody>
      {{range .Pools}}
        <tr>
          <td>{{.Name}}</td>
          <td class="{{healthClass .Health}}">{{.Health}}</td>
          <td>{{.Alloc}}</td>
          <td>{{.Size}}</td>
          <td>{{.Cap}}</td>
          <td>{{.Frag}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<p class="empty">No pools found.</p>{{end}}
  </section>

  <section>
    <h2>Scrubs</h2>
    {{if .Scrubs}}
    <table>
      <thead><tr><th>Pool</th><th>Progress</th><th>Done</th><th>Time remaining</th></tr></thead>
      <tbody>
      {{range .Scrubs}}
        <tr>
          <td>{{.Pool}}</td>
          <td><span class="bar"><span style="width:{{.Percent}}"></span></span></td>
          <td>{{if .Percent}}{{.Percent}}{{else}}&mdash;{{end}}</td>
          <td>{{if .ToGo}}{{.ToGo}}{{else}}&mdash;{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<p class="empty">No scrubs running.</p>{{end}}
  </section>

  <section>
    <h2>Datasets</h2>
    {{if .Datasets}}
    <table>
      <thead><tr><th>Name</th><th>Used</th><th>Available</th><th>Compression</th></tr></thead>
      <tbody>
      {{range .Datasets}}
        <tr>
          <td>{{.Name}}</td>
          <td>{{.Used}}</td>
          <td>{{.Avail}}</td>
          <td>{{.Ratio}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<p class="empty">No datasets found.</p>{{end}}
  </section>
</main>
</body>
</html>`))

// healthClass maps a pool health string to a CSS class for colouring.
func healthClass(health string) string {
	switch health {
	case "ONLINE":
		return "online"
	case "DEGRADED":
		return "degraded"
	case "FAULTED":
		return "faulted"
	case "UNAVAIL":
		return "unavail"
	case "OFFLINE":
		return "offline"
	default:
		return ""
	}
}

// Serve runs the dashboard HTTP server until the process is stopped.
func Serve(cfg Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard(cfg))

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("zpulse listening on http://localhost%s (showing %s)", addr, poolsLabel(cfg.Pools))
	return http.ListenAndServe(addr, mux)
}

func handleDashboard(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		st := Collect(cfg.Pools)

		data := struct {
			Status
			Refresh int
		}{
			Status:  st,
			Refresh: cfg.RefreshInterval,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboard.Execute(w, data); err != nil {
			log.Printf("render error: %v", err)
		}
	}
}

func poolsLabel(pools []string) string {
	if len(pools) == 0 {
		return "all pools"
	}
	return fmt.Sprintf("pools %v", pools)
}
