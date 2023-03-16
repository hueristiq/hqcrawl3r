package hqcrawl3r

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/debug"
	"github.com/gocolly/colly/v2/extensions"
	hqurl "github.com/hueristiq/hqgoutils/url"
)

type Crawler struct {
	URL                   *hqurl.URL
	Configuration         *Options
	PageCollector         *colly.Collector
	LinkFindCollector     *colly.Collector
	URLsToLinkFindRegex   *regexp.Regexp
	URLsNotToRequestRegex *regexp.Regexp
}

type Options struct {
	TargetURL         *hqurl.URL
	AllowedDomains    []string
	Concurrency       int
	Cookie            string
	Debug             bool
	Delay             int
	Depth             int
	Headers           string
	IncludeSubdomains bool
	MaxRandomDelay    int // seconds
	Proxy             string
	RenderTimeout     int // seconds
	Threads           int
	Timeout           int // seconds
	UserAgent         string
}

var foundURLs sync.Map
var visitedURLs sync.Map

func New(options *Options) (crawler Crawler, err error) {
	crawler.URL = options.TargetURL
	crawler.Configuration = options

	if options.AllowedDomains == nil {
		options.AllowedDomains = []string{}
	}

	options.AllowedDomains = append(options.AllowedDomains, []string{crawler.URL.Domain, "www." + crawler.URL.Domain}...)

	crawler.PageCollector = colly.NewCollector(
		colly.IgnoreRobotsTxt(),
		colly.AllowedDomains(options.AllowedDomains...),
		colly.MaxDepth(crawler.Configuration.Depth),
		colly.Async(true),
		colly.AllowURLRevisit(),
	)

	// if -subs is present, use regex to filter out subdomains in scope.
	if crawler.Configuration.IncludeSubdomains {
		crawler.PageCollector.AllowedDomains = nil
		crawler.PageCollector.URLFilters = []*regexp.Regexp{
			regexp.MustCompile(".*(\\.|\\/\\/)" + strings.ReplaceAll(crawler.URL.Domain, ".", "\\.") + "((#|\\/|\\?).*)?"),
		}
	}

	// Debug
	if crawler.Configuration.Debug {
		crawler.PageCollector.SetDebugger(&debug.LogDebugger{})
	}

	// Setup the client with our transport to pass to the collectors
	// NOTE: Must come BEFORE .SetClient calls
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(crawler.Configuration.Timeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:    100,
		MaxConnsPerHost: 1000,
		IdleConnTimeout: 30 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			Renegotiation:      tls.RenegotiateOnceAsClient,
		},
	}

	if crawler.Configuration.Proxy != "" {
		pU, err := url.Parse(crawler.Configuration.Proxy)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			transport.Proxy = http.ProxyURL(pU)
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(crawler.Configuration.Timeout) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			nextLocation := req.Response.Header.Get("Location")

			if strings.Contains(nextLocation, crawler.URL.Hostname()) {
				return nil
			}

			return http.ErrUseLastResponse
		},
	}

	crawler.PageCollector.SetClient(client)

	// set cookie
	if crawler.Configuration.Cookie != "" {
		crawler.PageCollector.OnRequest(func(request *colly.Request) {
			request.Headers.Set("Cookie", crawler.Configuration.Cookie)
		})
	}

	// set headers
	if crawler.Configuration.Headers != "" {
		crawler.PageCollector.OnRequest(func(request *colly.Request) {
			headers := strings.Split(crawler.Configuration.Headers, ";;")
			for _, header := range headers {
				var parts []string

				if strings.Contains(header, ": ") {
					parts = strings.SplitN(header, ": ", 2)
				} else if strings.Contains(header, ":") {
					parts = strings.SplitN(header, ":", 2)
				} else {
					continue
				}

				request.Headers.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
			}
		})
	}

	// Set User-Agent
	switch ua := strings.ToLower(crawler.Configuration.UserAgent); {
	case strings.HasPrefix(ua, "mobi"):
		extensions.RandomMobileUserAgent(crawler.PageCollector)
	case strings.HasPrefix(ua, "web"):
		extensions.RandomUserAgent(crawler.PageCollector)
	default:
		crawler.PageCollector.UserAgent = ua
	}

	// Referer
	extensions.Referer(crawler.PageCollector)

	// Set parallelism
	if err = crawler.PageCollector.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: crawler.Configuration.Concurrency,
		Delay:       time.Duration(crawler.Configuration.Delay) * time.Second,
		RandomDelay: time.Duration(crawler.Configuration.MaxRandomDelay) * time.Second,
	}); err != nil {
		return
	}

	crawler.LinkFindCollector = crawler.PageCollector.Clone()
	crawler.LinkFindCollector.URLFilters = nil

	crawler.PageCollector.ID = 1
	crawler.LinkFindCollector.ID = 2

	crawler.URLsToLinkFindRegex = regexp.MustCompile(`(?m).*?\.*(js|json|xml|csv|txt|map)(\?.*?|)$`)
	crawler.URLsNotToRequestRegex = regexp.MustCompile(`(?i)\.(apng|bpm|png|bmp|gif|heif|ico|cur|jpg|jpeg|jfif|pjp|pjpeg|psd|raw|svg|tif|tiff|webp|xbm|3gp|aac|flac|mpg|mpeg|mp3|mp4|m4a|m4v|m4p|oga|ogg|ogv|mov|wav|webm|eot|woff|woff2|ttf|otf|css)(?:\?|#|$)`)

	return crawler, nil
}

func (crawler *Crawler) Crawl() (results chan string, err error) {
	crawler.PageCollector.OnRequest(func(request *colly.Request) {
		URL := strings.TrimRight(request.URL.String(), "/")

		if _, exists := visitedURLs.Load(URL); exists {
			request.Abort()

			return
		}

		if match := crawler.URLsNotToRequestRegex.MatchString(URL); match {
			request.Abort()

			return
		}

		if match := crawler.URLsToLinkFindRegex.MatchString(URL); match {
			if err = crawler.LinkFindCollector.Visit(URL); err != nil {
				fmt.Println(err)
			}

			request.Abort()

			return
		}

		visitedURLs.Store(URL, struct{}{})

		return
	})

	crawler.LinkFindCollector.OnResponse(func(response *colly.Response) {
		URL := strings.TrimRight(response.Request.URL.String(), "/")

		if _, exists := foundURLs.Load(URL); !exists {
			return
		}

		if err := crawler.record(URL); err != nil {
			return
		}

		foundURLs.Store(URL, struct{}{})
	})

	crawler.PageCollector.OnHTML("[href]", func(e *colly.HTMLElement) {
		relativeURL := e.Attr("href")
		absoluteURL := e.Request.AbsoluteURL(relativeURL)

		if _, exists := foundURLs.Load(absoluteURL); exists {
			return
		}

		if err := crawler.record(absoluteURL); err != nil {
			return
		}

		foundURLs.Store(absoluteURL, struct{}{})

		if _, exists := visitedURLs.Load(absoluteURL); !exists {
			if err = e.Request.Visit(relativeURL); err != nil {
				return
			}
		}
	})

	crawler.PageCollector.OnHTML("[src]", func(e *colly.HTMLElement) {
		relativeURL := e.Attr("src")
		absoluteURL := e.Request.AbsoluteURL(relativeURL)

		if _, exists := foundURLs.Load(absoluteURL); exists {
			return
		}

		if err := crawler.record(absoluteURL); err != nil {
			return
		}

		foundURLs.Store(absoluteURL, struct{}{})

		if _, exists := visitedURLs.Load(absoluteURL); !exists {
			if err = e.Request.Visit(relativeURL); err != nil {
				return
			}
		}
	})

	crawler.LinkFindCollector.OnRequest(func(request *colly.Request) {
		URL := request.URL.String()

		if _, exists := visitedURLs.Load(URL); exists {
			request.Abort()

			return
		}

		// If the URL is a `.min.js` (Minified JavaScript) try finding `.js`
		if strings.Contains(URL, ".min.js") {
			js := strings.ReplaceAll(URL, ".min.js", ".js")

			if _, exists := visitedURLs.Load(js); !exists {
				if err = crawler.LinkFindCollector.Visit(js); err != nil {
					return
				}

				visitedURLs.Store(js, struct{}{})
			}
		}

		visitedURLs.Store(URL, struct{}{})
	})

	crawler.LinkFindCollector.OnResponse(func(response *colly.Response) {
		links, err := crawler.FindLinks(string(response.Body))
		if err != nil {
			return
		}

		if len(links) < 1 {
			return
		}

		for _, link := range links {
			// Skip blank entries
			if len(link) <= 0 {
				continue
			}

			// Remove the single and double quotes from the parsed link on the ends
			link = strings.Trim(link, "\"")
			link = strings.Trim(link, "'")

			// Get the absolute URL
			absoluteURL := response.Request.AbsoluteURL(link)

			// Trim the trailing slash
			absoluteURL = strings.TrimRight(absoluteURL, "/")

			// Trim the spaces on either end (if any)
			absoluteURL = strings.Trim(absoluteURL, " ")
			if absoluteURL == "" {
				return
			}

			URL := crawler.fixURL(absoluteURL)

			if _, exists := foundURLs.Load(URL); !exists {
				if err := crawler.record(URL); err != nil {
					return
				}

				foundURLs.Store(URL, struct{}{})
			}

			if _, exists := visitedURLs.Load(URL); !exists {
				if err = crawler.PageCollector.Visit(URL); err != nil {
					return
				}
			}
		}
	})

	if err = crawler.PageCollector.Visit(crawler.URL.String()); err != nil {
		return
	}

	// Async means we must .Wait() on each Collector
	crawler.PageCollector.Wait()
	crawler.LinkFindCollector.Wait()

	return
}
