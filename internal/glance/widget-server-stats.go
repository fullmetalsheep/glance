package glance

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glanceapp/glance/pkg/sysinfo"
)

var serverStatsWidgetTemplate = mustParseTemplate("server-stats.html", "widget-base.html")

type serverStatsWidget struct {
	widgetBase `yaml:",inline"`
	Servers    []serverStatsRequest `yaml:"servers"`
}

func (widget *serverStatsWidget) initialize() error {
	widget.withTitle("Server Stats").withCacheDuration(15 * time.Second)
	widget.widgetBase.WIP = true

	if len(widget.Servers) == 0 {
		widget.Servers = []serverStatsRequest{{Type: "local"}}
	}

	for i := range widget.Servers {
		widget.Servers[i].URL = strings.TrimRight(widget.Servers[i].URL, "/")

		if widget.Servers[i].Timeout == 0 {
			widget.Servers[i].Timeout = durationField(3 * time.Second)
		}
	}

	return nil
}

// update must not block on network I/O since it runs in the page's
// synchronous update pass; the real fetch happens in refresh().
func (widget *serverStatsWidget) update(ctx context.Context) {
	if !widget.tryStartAsyncUpdate() {
		return
	}

	go widget.refresh(ctx)
}

func (widget *serverStatsWidget) refresh(context.Context) {
	// Refactor later, most of it may change depending on feedback
	defer func() {
		widget.mu.Lock()
		widget.updating = false
		widget.mu.Unlock()
	}()

	var wg sync.WaitGroup

	// buffered locally instead of writing widget.Servers[i] directly,
	// since that would race with a concurrent Render()
	results := make([]struct {
		isReachable bool
		info        *sysinfo.SystemInfo
	}, len(widget.Servers))

	for i := range widget.Servers {
		serv := &widget.Servers[i]

		if serv.Type == "local" {
			info, errs := sysinfo.Collect(serv.SystemInfoRequest)

			if len(errs) > 0 {
				for i := range errs {
					slog.Warn("Getting system info: " + errs[i].Error())
				}
			}

			results[i].isReachable = true
			results[i].info = info
		} else {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				info, err := fetchRemoteServerInfo(serv)
				if err != nil {
					slog.Warn("Getting remote system info: " + err.Error())
					results[i].isReachable = false
					results[i].info = &sysinfo.SystemInfo{
						Hostname: "Unnamed server #" + strconv.Itoa(i+1),
					}
				} else {
					results[i].isReachable = true
					results[i].info = info
				}
			}()
		}
	}

	wg.Wait()

	widget.mu.Lock()
	defer widget.mu.Unlock()

	widget.pending = false

	for i := range widget.Servers {
		widget.Servers[i].IsReachable = results[i].isReachable
		widget.Servers[i].Info = results[i].info
	}

	widget.withError(nil).scheduleNextUpdate()
}

func (widget *serverStatsWidget) Render() template.HTML {
	return widget.renderLocked(widget, serverStatsWidgetTemplate)
}

func (widget *serverStatsWidget) handleRequest(w http.ResponseWriter, r *http.Request) {
	writeWidgetContent(w, widget.Render())
}

type serverStatsRequest struct {
	*sysinfo.SystemInfoRequest `yaml:",inline"`
	Info                       *sysinfo.SystemInfo `yaml:"-"`
	IsReachable                bool                `yaml:"-"`
	StatusText                 string              `yaml:"-"`
	Name                       string              `yaml:"name"`
	HideSwap                   bool                `yaml:"hide-swap"`
	Type                       string              `yaml:"type"`
	URL                        string              `yaml:"url"`
	Token                      string              `yaml:"token"`
	Timeout                    durationField       `yaml:"timeout"`
	// Support for other agents
	// Provider                   string              `yaml:"provider"`
}

func fetchRemoteServerInfo(infoReq *serverStatsRequest) (*sysinfo.SystemInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(infoReq.Timeout))
	defer cancel()

	request, _ := http.NewRequestWithContext(ctx, "GET", infoReq.URL+"/api/sysinfo/all", nil)
	if infoReq.Token != "" {
		request.Header.Set("Authorization", "Bearer "+infoReq.Token)
	}

	info, err := decodeJsonFromRequest[*sysinfo.SystemInfo](defaultHTTPClient, request)
	if err != nil {
		return nil, err
	}

	return info, nil
}
