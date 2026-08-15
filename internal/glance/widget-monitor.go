package glance

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"time"
)

var (
	monitorWidgetTemplate        = mustParseTemplate("monitor.html", "widget-base.html")
	monitorWidgetCompactTemplate = mustParseTemplate("monitor-compact.html", "widget-base.html")
)

type monitorWidget struct {
	widgetBase `yaml:",inline"`
	Sites      []struct {
		*SiteStatusRequest `yaml:",inline"`
		Status             *siteStatus     `yaml:"-"`
		URL                string          `yaml:"-"`
		ErrorURL           string          `yaml:"error-url"`
		Title              string          `yaml:"title"`
		Icon               customIconField `yaml:"icon"`
		SameTab            bool            `yaml:"same-tab"`
		StatusText         string          `yaml:"-"`
		StatusStyle        string          `yaml:"-"`
		AltStatusCodes     []int           `yaml:"alt-status-codes"`
	} `yaml:"sites"`
	Style           string `yaml:"style"`
	ShowFailingOnly bool   `yaml:"show-failing-only"`
	HasFailing      bool   `yaml:"-"`
	HasPending      bool   `yaml:"-"`
}

func (widget *monitorWidget) initialize() error {
	widget.withTitle("Monitor").withCacheDuration(5 * time.Minute)

	return nil
}

// update is called synchronously as part of the page's blocking update
// pass, so it must never block on network I/O. The actual site checks
// are done in a detached goroutine (refreshSites) - on the very first
// run this leaves the widget showing "checking" placeholders, and on
// subsequent runs it leaves the previously fetched (stale) statuses in
// place until the background refresh completes.
func (widget *monitorWidget) update(_ context.Context) {
	widget.mu.Lock()

	if widget.updating {
		widget.mu.Unlock()
		return
	}
	widget.updating = true

	if !widget.ContentAvailable {
		for i := range widget.Sites {
			site := &widget.Sites[i]
			if site.Status == nil {
				site.Status = &siteStatus{Pending: true}
				site.StatusText = "Checking..."
				site.StatusStyle = "pending"
				site.URL = site.DefaultURL
			}
		}

		widget.HasPending = true
		widget.ContentAvailable = true
	}

	widget.mu.Unlock()

	go widget.refreshSites()
}

func (widget *monitorWidget) refreshSites() {
	requests := make([]*SiteStatusRequest, len(widget.Sites))

	for i := range widget.Sites {
		requests[i] = widget.Sites[i].SiteStatusRequest
	}

	statuses, err := fetchStatusForSites(requests)

	widget.mu.Lock()
	defer func() {
		widget.updating = false
		widget.mu.Unlock()
	}()

	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	widget.HasFailing = false
	widget.HasPending = false

	for i := range widget.Sites {
		site := &widget.Sites[i]
		status := &statuses[i]
		site.Status = status

		if !slices.Contains(site.AltStatusCodes, status.Code) && (status.Code >= 400 || status.Error != nil) {
			widget.HasFailing = true
		}

		if status.Error != nil && site.ErrorURL != "" {
			site.URL = site.ErrorURL
		} else {
			site.URL = site.DefaultURL
		}

		site.StatusText = statusCodeToText(status.Code, site.AltStatusCodes)
		site.StatusStyle = statusCodeToStyle(status.Code, site.AltStatusCodes)
	}
}

// IsPending is only ever called from within a template invoked by Render,
// which already holds widget.mu - locking here would deadlock.
func (widget *monitorWidget) IsPending() bool {
	return widget.HasPending
}

func (widget *monitorWidget) Render() template.HTML {
	if widget.Style == "compact" {
		return widget.renderLocked(widget, monitorWidgetCompactTemplate)
	}

	return widget.renderLocked(widget, monitorWidgetTemplate)
}

func (widget *monitorWidget) handleRequest(w http.ResponseWriter, r *http.Request) {
	writeWidgetContent(w, widget.Render())
}

func statusCodeToText(status int, altStatusCodes []int) string {
	if status == 200 || slices.Contains(altStatusCodes, status) {
		return "OK"
	}
	if status == 404 {
		return "Not Found"
	}
	if status == 403 {
		return "Forbidden"
	}
	if status == 401 {
		return "Unauthorized"
	}
	if status >= 500 {
		return "Server Error"
	}
	if status >= 400 {
		return "Client Error"
	}

	return strconv.Itoa(status)
}

func statusCodeToStyle(status int, altStatusCodes []int) string {
	if status == 200 || slices.Contains(altStatusCodes, status) {
		return "ok"
	}

	return "error"
}

type SiteStatusRequest struct {
	DefaultURL    string        `yaml:"url"`
	CheckURL      string        `yaml:"check-url"`
	AllowInsecure bool          `yaml:"allow-insecure"`
	Timeout       durationField `yaml:"timeout"`
	BasicAuth     struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"basic-auth"`
}

type siteStatus struct {
	Code         int
	TimedOut     bool
	ResponseTime time.Duration
	Error        error
	Pending      bool
}

func fetchSiteStatusTask(statusRequest *SiteStatusRequest) (siteStatus, error) {
	var url string
	if statusRequest.CheckURL != "" {
		url = statusRequest.CheckURL
	} else {
		url = statusRequest.DefaultURL
	}

	timeout := ternary(statusRequest.Timeout > 0, time.Duration(statusRequest.Timeout), 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return siteStatus{
			Error: err,
		}, nil
	}

	if statusRequest.BasicAuth.Username != "" || statusRequest.BasicAuth.Password != "" {
		request.SetBasicAuth(statusRequest.BasicAuth.Username, statusRequest.BasicAuth.Password)
	}

	requestSentAt := time.Now()
	var response *http.Response

	if !statusRequest.AllowInsecure {
		response, err = defaultHTTPClient.Do(request)
	} else {
		response, err = defaultInsecureHTTPClient.Do(request)
	}

	status := siteStatus{ResponseTime: time.Since(requestSentAt)}

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			status.TimedOut = true
		}

		status.Error = err
		return status, nil
	}

	defer response.Body.Close()

	status.Code = response.StatusCode

	return status, nil
}

func fetchStatusForSites(requests []*SiteStatusRequest) ([]siteStatus, error) {
	job := newJob(fetchSiteStatusTask, requests).withWorkers(20)
	results, _, err := workerPoolDo(job)
	if err != nil {
		return nil, err
	}

	return results, nil
}
