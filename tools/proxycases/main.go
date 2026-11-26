// Command proxycases writes the proxy-selection cases the TypeScript worker
// tests its agent downloads against: what Go's golang.org/x/net/http/httpproxy
// (the version in go.mod) chooses for each proxy setting, NO_PROXY value and
// URL below. The worker must choose the same; it refuses https:// and
// socks5:// proxies, which the cases still record as Go returns them.
//
// Regenerate after changing the inputs or upgrading golang.org/x/net:
//
//	go run ./tools/proxycases > worker/test/testdata/proxy-cases.json
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"golang.org/x/net/http/httpproxy"
)

// group is a set of proxy settings, NO_PROXY values and URLs, every
// combination of which is one case.
type group struct {
	proxies [][2]string // HTTP_PROXY, HTTPS_PROXY
	noProxy []string
	urls    []string
}

var groups = []group{{
	proxies: [][2]string{
		{"http://hp:3128", "http://sp:3128"}, {"hp:3128", "sp:3128"}, {"user:p%40ss@hp:3128", "socks5://sp:1080"},
		{"", "https://sp:443"}, {"localhost:3128", ""}, {"http://[::1]:3128", "[2001:db8::5]:3128"},
	},
	noProxy: []string{
		"", "*", "example.com", ".example.com", "*.example.com", "example.com:443", "example.com:8443", "10.0.0.0/8",
		"10.0.0.0/8:443", "10.1.2.3", "10.1.2.3:443", "1.2.3", "2001:db8::/32", "[2001:db8::1]", "2001:db8::1",
		"[2001:db8::1]:443", "[2001:db8::1]:80", "::ffff:0:0/96", " Example.COM , foo", "*.", "foo, *", "example.com:",
		"::1", ":443", "0.0.0.0/0", "EXAMPLE.com.", "xample.com", "registry.npmjs.org:443", "*example.com",
		"127.0.0.0/8", "10.0.0.0/33", "10.0.0.1/8", "a.b.c.d", "2.3.4",
	},
	urls: []string{
		"https://registry.npmjs.org/x.tgz", "https://example.com/x", "https://www.example.com:8443/x", "http://example.com/x",
		"https://10.1.2.3/x", "http://10.1.2.3:8080/x", "https://[2001:db8::1]/x", "http://[2001:db8::1]:80/x",
		"https://[::ffff:10.1.2.3]/x", "https://localhost/x", "http://127.0.0.5/x", "https://[::1]:8443/x",
		"https://sub.xample.com/x", "http://1.2.3.4/x", "https://EXAMPLE.com/x", "http://[::ffff:127.0.0.1]/x",
		"https://localhost.:443/x", "https://a.b.c.d/x",
	},
}, {
	// CIDR ranges whose prefix ends inside a byte.
	proxies: [][2]string{{"http://hp:3128", "http://sp:3128"}},
	noProxy: []string{
		"172.16.0.0/12", "10.0.0.0/20", "2001:db8::/33", "10.0.0.0/20,172.16.0.0/12:80", "::ffff:10.0.0.0/116",
		"10.0.0.0/0", "192.168.128.0/17", "2001:db8:8000::/35",
	},
	urls: []string{
		"https://172.31.255.1/x", "https://172.32.0.1/x", "https://172.15.255.255/x", "https://10.0.15.1/x",
		"https://10.0.16.1/x", "https://[2001:db8:7fff::1]/x", "https://[2001:db8:8000::1]/x", "https://[::ffff:10.0.15.1]/x",
		"http://172.20.0.1:80/x", "https://[2001:db9::1]/x", "https://192.168.127.255/x", "https://192.168.128.1/x",
		"https://192.168.255.255/x", "https://[2001:db8:9fff::1]/x", "https://[2001:db8:a000::1]/x",
	},
}}

// testCase is one case: the environment, the URL and Go's choice: "" for
// a direct connection, "error" when Go fails, else the proxy as
// scheme://host, with its decoded user and password in auth ("user:pass").
type testCase struct {
	HTTPProxy  string `json:"http_proxy"`
	HTTPSProxy string `json:"https_proxy"`
	NoProxy    string `json:"no_proxy"`
	URL        string `json:"url"`
	Proxy      string `json:"proxy"`
	Auth       string `json:"auth,omitempty"`
}

func main() {
	var out []testCase
	for _, g := range groups {
		for _, p := range g.proxies {
			for _, np := range g.noProxy {
				fn := (&httpproxy.Config{HTTPProxy: p[0], HTTPSProxy: p[1], NoProxy: np}).ProxyFunc()
				for _, raw := range g.urls {
					u, err := url.Parse(raw)
					if err != nil {
						fmt.Fprintln(os.Stderr, err)
						os.Exit(1)
					}
					c := testCase{HTTPProxy: p[0], HTTPSProxy: p[1], NoProxy: np, URL: raw}
					switch proxy, err := fn(u); {
					case err != nil:
						c.Proxy = "error"
					case proxy != nil:
						c.Proxy = proxy.Scheme + "://" + proxy.Host
						if proxy.User != nil {
							pass, _ := proxy.User.Password()
							c.Auth = proxy.User.Username() + ":" + pass
						}
					}
					out = append(out, c)
				}
			}
		}
	}
	// One case per line, so a regeneration diffs case by case.
	fmt.Println("[")
	for i, c := range out {
		line, err := json.Marshal(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sep := ","
		if i == len(out)-1 {
			sep = ""
		}
		fmt.Printf("%s%s\n", line, sep)
	}
	fmt.Println("]")
}
