package glance

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

var extensionWidgetTemplate = mustParseTemplate("extension.html", "widget-base.html")

const extensionWidgetDefaultTitle = "Extension"

type extensionWidget struct {
	widgetBase          `yaml:",inline"`
	URL                 string               `yaml:"url"`
	FallbackContentType string               `yaml:"fallback-content-type"`
	Parameters          queryParametersField `yaml:"parameters"`
	Headers             map[string]string    `yaml:"headers"`
	AllowHtml           bool                 `yaml:"allow-potentially-dangerous-html"`
	Extension           extension            `yaml:"-"`
	cachedHTML          template.HTML        `yaml:"-"`
}

func (widget *extensionWidget) initialize() error {
	widget.withTitle(extensionWidgetDefaultTitle).withCacheDuration(time.Minute * 30)

	if widget.URL == "" {
		return errors.New("URL is required")
	}

	if _, err := url.Parse(widget.URL); err != nil {
		return fmt.Errorf("parsing URL: %v", err)
	}

	return nil
}

// update must not block on network I/O since it runs in the page's
// synchronous update pass; the real fetch happens in refresh().
func (widget *extensionWidget) update(ctx context.Context) {
	if !widget.tryStartAsyncUpdate() {
		return
	}

	if widget.pending {
		// Render() serves cachedHTML directly, so it needs a placeholder
		// rendered now or the client would never see it's pending.
		widget.mu.Lock()
		widget.cachedHTML = widget.renderTemplate(widget, extensionWidgetTemplate)
		widget.mu.Unlock()
	}

	go widget.refresh(ctx)
}

func (widget *extensionWidget) refresh(_ context.Context) {
	extension, err := fetchExtension(extensionRequestOptions{
		URL:                 widget.URL,
		FallbackContentType: widget.FallbackContentType,
		Parameters:          widget.Parameters,
		Headers:             widget.Headers,
		AllowHtml:           widget.AllowHtml,
	})

	widget.mu.Lock()
	defer func() {
		widget.updating = false
		widget.mu.Unlock()
	}()

	widget.canContinueUpdateAfterHandlingErr(err)

	widget.pending = false
	widget.Extension = extension

	if widget.Title == extensionWidgetDefaultTitle && extension.Title != "" {
		widget.Title = extension.Title
	}

	if widget.TitleURL == "" && extension.TitleURL != "" {
		widget.TitleURL = extension.TitleURL
	}

	widget.cachedHTML = widget.renderTemplate(widget, extensionWidgetTemplate)
}

func (widget *extensionWidget) Render() template.HTML {
	widget.mu.Lock()
	defer widget.mu.Unlock()

	return widget.cachedHTML
}

func (widget *extensionWidget) handleRequest(w http.ResponseWriter, r *http.Request) {
	writeWidgetContent(w, widget.Render())
}

type extensionType int

const (
	extensionContentHTML extensionType = iota
	extensionContentUnknown
)

var extensionStringToType = map[string]extensionType{
	"html": extensionContentHTML,
}

const (
	extensionHeaderTitle            = "Widget-Title"
	extensionHeaderTitleURL         = "Widget-Title-URL"
	extensionHeaderContentType      = "Widget-Content-Type"
	extensionHeaderContentFrameless = "Widget-Content-Frameless"
)

type extensionRequestOptions struct {
	URL                 string               `yaml:"url"`
	FallbackContentType string               `yaml:"fallback-content-type"`
	Parameters          queryParametersField `yaml:"parameters"`
	Headers             map[string]string    `yaml:"headers"`
	AllowHtml           bool                 `yaml:"allow-potentially-dangerous-html"`
}

type extension struct {
	Title     string
	TitleURL  string
	Content   template.HTML
	Frameless bool
}

func convertExtensionContent(options extensionRequestOptions, content []byte, contentType extensionType) template.HTML {
	switch contentType {
	case extensionContentHTML:
		if options.AllowHtml {
			return template.HTML(content)
		}

		fallthrough
	default:
		return template.HTML("<pre>" + html.EscapeString(string(content)) + "</pre>")
	}
}

func fetchExtension(options extensionRequestOptions) (extension, error) {
	request, _ := http.NewRequest("GET", options.URL, nil)
	if len(options.Parameters) > 0 {
		request.URL.RawQuery = options.Parameters.toQueryString()
	}

	for key, value := range options.Headers {
		request.Header.Add(key, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		slog.Error("Failed fetching extension", "url", options.URL, "error", err)
		return extension{}, fmt.Errorf("%w: request failed: %w", errNoContent, err)
	}

	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		slog.Error("Failed reading response body of extension", "url", options.URL, "error", err)
		return extension{}, fmt.Errorf("%w: could not read body: %w", errNoContent, err)
	}

	extension := extension{}

	if response.Header.Get(extensionHeaderTitle) == "" {
		extension.Title = "Extension"
	} else {
		extension.Title = response.Header.Get(extensionHeaderTitle)
	}

	if response.Header.Get(extensionHeaderTitleURL) != "" {
		extension.TitleURL = response.Header.Get(extensionHeaderTitleURL)
	}

	contentType, ok := extensionStringToType[response.Header.Get(extensionHeaderContentType)]

	if !ok {
		contentType, ok = extensionStringToType[options.FallbackContentType]

		if !ok {
			contentType = extensionContentUnknown
		}
	}

	if stringToBool(response.Header.Get(extensionHeaderContentFrameless)) {
		extension.Frameless = true
	}

	extension.Content = convertExtensionContent(options, body, contentType)

	return extension, nil
}
