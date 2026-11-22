package webapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/store"
)

// The Web UI runs the rules in rules.go before it sends anything (see
// web/src/api/validation.ts and git-url.ts). TestWebParityFixture keeps the
// two in step: it writes what the Go rules return for webParityInputs to
// web/src/api/validation-parity.json, which the Web's parity test runs
// against its own port, and the simple case-folding orbits that project
// name uniqueness uses to web/src/api/case-fold.json. Without -update it
// fails when either file differs from what the rules return now, so a
// change to the rules fails here until the fixtures are regenerated:
//
//	go test ./internal/controlplane/webapi -run TestWebParityFixture -update
//
// and then the Web's tests show whether its port still agrees.

var updateWebFixtures = flag.Bool("update", false, "rewrite the Web UI's parity fixtures")

const (
	webParityFixture = "../../../web/src/api/validation-parity.json"
	webCaseFoldTable = "../../../web/src/api/case-fold.json"
)

// webParityInputs are the inputs of the Web's parity cases: every call the
// tests in rules_test.go make to the checked functions, and more inputs for
// the corners of net/url, net/netip and case folding the rules rely on. Add
// a case here when adding one to rules_test.go.
type webParityInput struct {
	fn   string
	args []string
}

var webParityInputs = []webParityInput{
	{"normalizePath", []string{"/src/a/", "linux"}},
	{"normalizePath", []string{"/src/a", "linux"}},
	{"normalizePath", []string{" /a ", "darwin"}},
	{"normalizePath", []string{"/", "linux"}},
	{"normalizePath", []string{"///", "linux"}},
	{"normalizePath", []string{"/a//", "linux"}},
	{"normalizePath", []string{"src/relative", "linux"}},
	{"normalizePath", []string{"", "linux"}},
	{"normalizePath", []string{"   ", "linux"}},
	{"normalizePath", []string{"C:\\src", "linux"}},
	{"normalizePath", []string{"/x/", ""}},
	{"normalizePath", []string{"/x/", "freebsd"}},
	{"normalizePath", []string{"\u00a0/a\u3000", "linux"}},
	{"normalizePath", []string{"\ufeff/a", "linux"}},
	{"normalizePath", []string{"\u0085/a", "linux"}},
	{"normalizePath", []string{"c:/src/app", "windows"}},
	{"normalizePath", []string{"D:\\work", "windows"}},
	{"normalizePath", []string{"d:/work/", "windows"}},
	{"normalizePath", []string{"D:\\src", "windows"}},
	{"normalizePath", []string{"C:", "windows"}},
	{"normalizePath", []string{"c:\\", "windows"}},
	{"normalizePath", []string{"c://", "windows"}},
	{"normalizePath", []string{"//server/share/", "windows"}},
	{"normalizePath", []string{"\\\\server\\share\\dir\\", "windows"}},
	{"normalizePath", []string{"/not/windows", "windows"}},
	{"normalizePath", []string{"e:/x", "Windows"}},
	{"normalizePath", []string{"1:/x", "windows"}},
	{"checkPath", []string{"/src", "linux"}},
	{"checkPath", []string{"/src/a/", "linux"}},
	{"checkPath", []string{"/Users/me", "darwin"}},
	{"checkPath", []string{"/", "linux"}},
	{"checkPath", []string{"D:\\src", "windows"}},
	{"checkPath", []string{"D:\\work", "windows"}},
	{"checkPath", []string{"c:/src/app", "windows"}},
	{"checkPath", []string{"C:", "windows"}},
	{"checkPath", []string{"\\\\server\\share", "windows"}},
	{"checkPath", []string{"//server/share/x", "windows"}},
	{"checkPath", []string{"/" + strings.Repeat("x", 4095), "linux"}},
	{"checkPath", []string{"", "linux"}},
	{"checkPath", []string{"  ", "linux"}},
	{"checkPath", []string{"", "windows"}},
	{"checkPath", []string{"\u3000", "windows"}},
	{"checkPath", []string{"src", "linux"}},
	{"checkPath", []string{"src/relative", "linux"}},
	{"checkPath", []string{"~/src", "linux"}},
	{"checkPath", []string{"\u0085/a", "linux"}},
	{"checkPath", []string{"/src", "windows"}},
	{"checkPath", []string{"/not/windows", "windows"}},
	{"checkPath", []string{"src\\app", "windows"}},
	{"checkPath", []string{"C:src", "windows"}},
	{"checkPath", []string{"\\\\server", "windows"}},
	{"checkPath", []string{"\\\\server\\", "windows"}},
	{"checkPath", []string{"\\\\\\server\\share", "windows"}},
	{"checkPath", []string{"\\\\", "windows"}},
	{"checkPath", []string{"\\\\\\", "windows"}},
	{"checkPath", []string{"/a\u0000b", "linux"}},
	{"checkPath", []string{"/" + strings.Repeat("x", 4096), "linux"}},
	{"checkName", []string{"  web  "}},
	{"checkName", []string{"circus"}},
	{"checkName", []string{strings.Repeat("x", 64)}},
	{"checkName", []string{strings.Repeat("\U0001f600", 64)}},
	{"checkName", []string{"\u3000\u540d\u524d\ufeff"}},
	{"checkName", []string{"\ufeffweb\u00a0"}},
	{"checkName", []string{"a b"}},
	{"checkName", []string{""}},
	{"checkName", []string{"   "}},
	{"checkName", []string{"\u00a0\u2028"}},
	{"checkName", []string{strings.Repeat("x", 65)}},
	{"checkName", []string{strings.Repeat("\U0001f600", 65)}},
	{"checkName", []string{"a\u0009b"}},
	{"checkName", []string{"a\u0000"}},
	{"checkName", []string{"a\u007fb"}},
	{"checkName", []string{"a\u000ab"}},
	{"checkName", []string{"a\u0085b"}},
	{"checkName", []string{"a\u009fb"}},
	{"checkName", []string{"a\u200bb"}},
	{"checkName", []string{"a\u200db"}},
	{"checkName", []string{"a\ufeffb"}},
	{"checkName", []string{"a\u202eb"}},
	{"checkName", []string{"a\u202ab"}},
	{"checkName", []string{"a\u2066b"}},
	{"checkName", []string{"a\u2069b"}},
	{"checkName", []string{"a\u200eb"}},
	{"checkName", []string{"a\u200fb"}},
	{"checkName", []string{"a\u061cb"}},
	{"checkName", []string{"a\u2028b"}},
	{"checkName", []string{"a\u2029b"}},
	{"checkRepoURL", []string{"https://github.com/owner/repo.git"}},
	{"checkRepoURL", []string{"https://github.com/owner/repo"}},
	{"checkRepoURL", []string{"http://git.example.com/x"}},
	{"checkRepoURL", []string{"https://[::1]:8080/x.git"}},
	{"checkRepoURL", []string{"ssh://git@github.com/owner/repo.git"}},
	{"checkRepoURL", []string{"ssh://github.com/owner/repo.git"}},
	{"checkRepoURL", []string{"ssh://git@host:2222/srv/repo.git"}},
	{"checkRepoURL", []string{"git://host.example/repo.git"}},
	{"checkRepoURL", []string{"file:///srv/git/repo.git"}},
	{"checkRepoURL", []string{"git@github.com:owner/repo.git"}},
	{"checkRepoURL", []string{"git@host:repo"}},
	{"checkRepoURL", []string{"user@10.0.0.1:/abs/repo.git"}},
	{"checkRepoURL", []string{"https://Host/Path"}},
	{"checkRepoURL", []string{"  git@github.com:Owner/Repo.git\u000a"}},
	{"checkRepoURL", []string{" \u0009 "}},
	{"checkRepoURL", []string{"github.com/owner/repo"}},
	{"checkRepoURL", []string{"owner/repo"}},
	{"checkRepoURL", []string{"/srv/git/repo.git"}},
	{"checkRepoURL", []string{"host:repo"}},
	{"checkRepoURL", []string{"@host:repo"}},
	{"checkRepoURL", []string{"git@:repo"}},
	{"checkRepoURL", []string{"git@host:"}},
	{"checkRepoURL", []string{"a/b@host:repo"}},
	{"checkRepoURL", []string{"-oProxyCommand=x@host:repo"}},
	{"checkRepoURL", []string{"git@-host:repo"}},
	{"checkRepoURL", []string{"--upload-pack=touch /tmp/x"}},
	{"checkRepoURL", []string{"https://"}},
	{"checkRepoURL", []string{"https://host"}},
	{"checkRepoURL", []string{"https://host/"}},
	{"checkRepoURL", []string{"https:///path"}},
	{"checkRepoURL", []string{"https://user@host/x"}},
	{"checkRepoURL", []string{"https://user:secret@host/x"}},
	{"checkRepoURL", []string{"ssh://user:secret@host/x"}},
	{"checkRepoURL", []string{"ssh://-oProxyCommand=x/repo"}},
	{"checkRepoURL", []string{"ssh://-user@host/repo"}},
	{"checkRepoURL", []string{"ssh://host:port/x"}},
	{"checkRepoURL", []string{"https://host/x?ref=main"}},
	{"checkRepoURL", []string{"https://host/x#frag"}},
	{"checkRepoURL", []string{"https://host/x#"}},
	{"checkRepoURL", []string{"ftp://host/x"}},
	{"checkRepoURL", []string{"HTTPS://host/x"}},
	{"checkRepoURL", []string{"ext::nc%20host"}},
	{"checkRepoURL", []string{"file://host/x"}},
	{"checkRepoURL", []string{"file:///"}},
	{"checkRepoURL", []string{"file:relative"}},
	{"checkRepoURL", []string{"ext::sh -c touch% /tmp/pwned"}},
	{"checkRepoURL", []string{"https://host/a b"}},
	{"checkRepoURL", []string{"https://host/a\u0009b"}},
	{"checkRepoURL", []string{"https://host/a\u0085b"}},
	{"checkRepoURL", []string{"https://host/a\u007f"}},
	{"checkRepoURL", []string{"https://host/a\u00a0b"}},
	{"checkRepoURL", []string{"https://host/" + strings.Repeat("x", 2048)}},
	{"normalizePath", []string{"/a//b", "linux"}},
	{"normalizePath", []string{"/a/./b/", "linux"}},
	{"normalizePath", []string{"//a", "linux"}},
	{"normalizePath", []string{"/./", "linux"}},
	{"normalizePath", []string{"/.", "linux"}},
	{"normalizePath", []string{"/a/.", "linux"}},
	{"normalizePath", []string{"/a/../b", "linux"}},
	{"normalizePath", []string{"/a/.b/..c", "linux"}},
	{"normalizePath", []string{"/a/./../b//", "linux"}},
	{"normalizePath", []string{"rel//x/", "linux"}},
	{"normalizePath", []string{"c:", "windows"}},
	{"checkPath", []string{"\\\\?\\C:\\x", "windows"}},
	{"checkPath", []string{"\\\\?\\UNC\\server\\share", "windows"}},
	{"checkPath", []string{"\\\\.\\PhysicalDrive0", "windows"}},
	{"checkPath", []string{"//?/C:/x", "windows"}},
	{"checkPath", []string{"\\\\.\\", "windows"}},
	{"checkPath", []string{"\\\\?", "windows"}},
	{"checkPath", []string{"/a\u0085b", "linux"}},
	{"checkPath", []string{"/a\u200bb", "linux"}},
	{"checkPath", []string{"/a\u202eb", "linux"}},
	{"checkPath", []string{"/a\ufeffb", "linux"}},
	{"checkPath", []string{"/a\u0001", "linux"}},
	{"checkPath", []string{"C:\\a\u2067b", "windows"}},
	{"checkPath", []string{"/a\u200eb", "linux"}},
	{"checkPath", []string{"/a\u200fb", "linux"}},
	{"checkPath", []string{"/a\u061cb", "linux"}},
	{"checkPath", []string{"/a\u2028b", "linux"}},
	{"checkPath", []string{"C:\\a\u2029b", "windows"}},
	{"checkPath", []string{"\\\\?x\\share", "windows"}},
	{"checkRepoURL", []string{"a.b_c~d-e@host-1.example:x"}},
	{"checkRepoURL", []string{"ssh://git@[2001:db8::1]:22/repo.git"}},
	{"checkRepoURL", []string{"ssh://[::1]/repo.git"}},
	{"checkRepoURL", []string{"ssh://deploy_user@git.example.com:65535/r"}},
	{"checkRepoURL", []string{"git@[::1]:repo"}},
	{"checkRepoURL", []string{"git@host!:repo"}},
	{"checkRepoURL", []string{"g%74@host:repo"}},
	{"checkRepoURL", []string{"us+er@host:repo"}},
	{"checkRepoURL", []string{"git@ho$t:repo"}},
	{"checkRepoURL", []string{"git@host,evil:repo"}},
	{"checkRepoURL", []string{"ssh://git@ho%73t/repo"}},
	{"checkRepoURL", []string{"ssh://us+er@host/repo"}},
	{"checkRepoURL", []string{"ssh://git@[fe80::1%25eth0]/repo"}},
	{"checkRepoURL", []string{"ssh://git@[1.2.3.4]/repo"}},
	{"checkRepoURL", []string{"ssh://git@host:0/repo"}},
	{"checkRepoURL", []string{"ssh://git@host:65536/repo"}},
	{"checkRepoURL", []string{"ssh://git@-host/repo"}},
	{"checkRepoURL", []string{"https://host/a\u200bb"}},
	{"checkRepoURL", []string{"https://host/a\u202eb"}},
	{"checkRepoURL", []string{"https://host/a\ufeffb"}},
	{"checkRepoURL", []string{"https://host/a\u200eb"}},
	{"checkRepoURL", []string{"https://host/a\u200fb"}},
	{"checkRepoURL", []string{"git@host:a\u061cb"}},
	{"checkRepoURL", []string{"https://host/a\u2028b"}},
	{"checkRepoURL", []string{"ssh://git@host/a\u2029b"}},
	{"checkName", []string{"\u2028web\u2029"}},
	{"checkPath", []string{"\u2029/a\u2028", "linux"}},
	{"checkName", []string{"a\u00a0b"}},
	{"checkName", []string{"a\u3000b"}},
	{"checkName", []string{strings.Repeat("x", 63) + "\U0001f600"}},
	{"checkName", []string{"\u200b"}},
	{"checkName", []string{"a\u180eb"}},
	{"checkName", []string{"a\u2060b"}},
	{"checkName", []string{"\u0085a"}},
	{"checkName", []string{"a\u0080b"}},
	{"checkName", []string{"\u540d\u524d"}},
	{"checkName", []string{" \u0009\u000a"}},
	{"checkName", []string{"a\u206ab"}},
	{"checkName", []string{"a\u2065b"}},
	{"checkPath", []string{"/a/./b", "linux"}},
	{"checkPath", []string{"//", "linux"}},
	{"checkPath", []string{"/.", "darwin"}},
	{"checkPath", []string{"/..", "linux"}},
	{"checkPath", []string{"/a/..", "linux"}},
	{"checkPath", []string{"\\\\?\\x", "windows"}},
	{"checkPath", []string{"//./x", "windows"}},
	{"checkPath", []string{"C:/a/./b", "windows"}},
	{"checkPath", []string{"/a\u00a0b", "linux"}},
	{"checkPath", []string{"\u00a0", "linux"}},
	{"checkPath", []string{"/x//y/", "darwin"}},
	{"checkPath", []string{"/x", ""}},
	{"checkPath", []string{"c:", "windows"}},
	{"checkPath", []string{"C:\\a\\", "windows"}},
	{"checkPath", []string{"//" + strings.Repeat("x", 4095), "linux"}},
	{"checkPath", []string{"/" + strings.Repeat("\u00e9", 2048), "linux"}},
	{"checkPath", []string{"\\\\server/share\\x/", "windows"}},
	{"checkPath", []string{"\\\\.", "windows"}},
	{"checkPath", []string{"\\\\?\\", "windows"}},
	{"checkPath", []string{"\\\\..\\share", "windows"}},
	{"checkPath", []string{"~/src", "darwin"}},
	{"checkPath", []string{"C:\\a\u200bb", "windows"}},
	{"checkPath", []string{"/a b", "linux"}},
	{"checkPath", []string{"/a\u0009b", "linux"}},
	{"checkPath", []string{" /a\u0009", "linux"}},
	{"checkPath", []string{"\u2028", "linux"}},
	{"checkPath", []string{"/a/\u3000", "linux"}},
	{"checkPath", []string{"x:/y", "windows"}},
	{"checkPath", []string{"\\\\s\\h", "WINDOWS"}},
	{"checkPath", []string{"/x", "Linux"}},
	{"checkPath", []string{"/a/./", "plan9"}},
	{"checkRepoURL", []string{"ssh://g%74@host/x"}},
	{"checkRepoURL", []string{"ssh://git@host:/x"}},
	{"checkRepoURL", []string{"https://host:/x"}},
	{"checkRepoURL", []string{"https://host:80:90/x"}},
	{"checkRepoURL", []string{"git://a:b:1/x"}},
	{"checkRepoURL", []string{"ssh://a:b:1/x"}},
	{"checkRepoURL", []string{"https://a:b:1/x"}},
	{"checkRepoURL", []string{"http://h:1:2/x"}},
	{"checkRepoURL", []string{"ssh://h:22:1/x"}},
	{"checkRepoURL", []string{"git://h:1:2/x"}},
	{"checkRepoURL", []string{"https://h::1/x"}},
	{"checkRepoURL", []string{"ssh://git@h::22/x"}},
	{"checkRepoURL", []string{"https://h:443/x"}},
	{"checkRepoURL", []string{"http://h:1/x"}},
	{"checkRepoURL", []string{"git://h:9418/x"}},
	{"checkRepoURL", []string{"ssh://h:22/x"}},
	{"checkRepoURL", []string{"https://h:65535/x"}},
	{"checkRepoURL", []string{"https://h:0/x"}},
	{"checkRepoURL", []string{"git://h:65536/x"}},
	{"checkRepoURL", []string{"https://h:+1/x"}},
	{"checkRepoURL", []string{"http://h:0443/x"}},
	{"checkRepoURL", []string{"https://u@h:1:2/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25eth0]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%eth0]/x"}},
	{"checkRepoURL", []string{"https://[1.2.3.4]/x"}},
	{"checkRepoURL", []string{"https://[::ffff:1.2.3.4]/x"}},
	{"checkRepoURL", []string{"ssh://[::ffff:1.2.3.4]/x"}},
	{"checkRepoURL", []string{"https://ho$t/x"}},
	{"checkRepoURL", []string{"https://ho%41t/x"}},
	{"checkRepoURL", []string{"https://ho%C3%A9t/x"}},
	{"checkRepoURL", []string{"https://host/%zz"}},
	{"checkRepoURL", []string{"https://host/%41"}},
	{"checkRepoURL", []string{"https://host/x?"}},
	{"checkRepoURL", []string{"https://host/a?b"}},
	{"checkRepoURL", []string{"https://-host/x"}},
	{"checkRepoURL", []string{"https://host?x"}},
	{"checkRepoURL", []string{"ssh://user@host"}},
	{"checkRepoURL", []string{"ssh://@host/x"}},
	{"checkRepoURL", []string{"ssh://user:@host/x"}},
	{"checkRepoURL", []string{"file:///a"}},
	{"checkRepoURL", []string{"file:////a"}},
	{"checkRepoURL", []string{"file:///%"}},
	{"checkRepoURL", []string{"http://h/\u00e9"}},
	{"checkRepoURL", []string{"https://h\u00e9/x"}},
	{"checkRepoURL", []string{"ssh://h\u00e9/x"}},
	{"checkRepoURL", []string{"git@h\u00e9:x"}},
	{"checkRepoURL", []string{"git@host:x://y"}},
	{"checkRepoURL", []string{"ssh://git@host:022/x"}},
	{"checkRepoURL", []string{"ssh://git@host:" + strings.Repeat("9", 20) + "/x"}},
	{"checkRepoURL", []string{"https://[::1/x"}},
	{"checkRepoURL", []string{"https://a]b/x"}},
	{"checkRepoURL", []string{"https://a[b]/x"}},
	{"checkRepoURL", []string{"https://[::1]x/x"}},
	{"checkRepoURL", []string{"https://[]/x"}},
	{"checkRepoURL", []string{"ssh://[::1%25lo]/x"}},
	{"checkRepoURL", []string{"https://h/" + strings.Repeat("x", 2038)}},
	{"checkRepoURL", []string{"https://h/" + strings.Repeat("x", 2039)}},
	{"checkRepoURL", []string{"Https://h/x"}},
	{"checkRepoURL", []string{"ssh+git://h/x"}},
	{"checkRepoURL", []string{"https:/host/x"}},
	{"checkRepoURL", []string{"https:host/x"}},
	{"checkRepoURL", []string{"ssh://host/x/../y"}},
	{"checkRepoURL", []string{"git@host:-x"}},
	{"checkRepoURL", []string{"a@b:c"}},
	{"checkRepoURL", []string{"user@@host:x"}},
	{"checkRepoURL", []string{"https://host/a\u3000b"}},
	{"checkRepoURL", []string{"https://host/a\u1680b"}},
	{"checkRepoURL", []string{"\u00a0https://h/x\u00a0"}},
	{"checkRepoURL", []string{"https://h/x\u180e"}},
	{"checkRepoURL", []string{"https://h/x\u2060"}},
	{"checkRepoURL", []string{"https://u%40v@h/x"}},
	{"checkRepoURL", []string{"ssh://u%40v@h/x"}},
	{"checkRepoURL", []string{"ssh://u%2@h/x"}},
	{"checkRepoURL", []string{"ssh://u@v@h/x"}},
	{"checkRepoURL", []string{"ssh://a!b@h/x"}},
	{"checkRepoURL", []string{"ssh://git@[::1]:22/x"}},
	{"checkRepoURL", []string{"ssh://git@[::1]:0/x"}},
	{"checkRepoURL", []string{"ssh://git@[::1:2:3:4:5:6:7:8]/x"}},
	{"checkRepoURL", []string{"ssh://[1::2::3]/x"}},
	{"checkRepoURL", []string{"ssh://[::1.2.3.4]/x"}},
	{"checkRepoURL", []string{"ssh://[::1.2.3.04]/x"}},
	{"checkRepoURL", []string{"ssh://[::1.2.3.256]/x"}},
	{"checkRepoURL", []string{"ssh://[1:2:3:4:5:6:1.2.3.4]/x"}},
	{"checkRepoURL", []string{"ssh://[1:2:3:4:5:6:7:1.2.3.4]/x"}},
	{"checkRepoURL", []string{"ssh://[12345::1]/x"}},
	{"checkRepoURL", []string{"ssh://[::]/x"}},
	{"checkRepoURL", []string{"ssh://[1::]/x"}},
	{"checkRepoURL", []string{"ssh://[:1]/x"}},
	{"checkRepoURL", []string{"ssh://[1:]/x"}},
	{"checkRepoURL", []string{"ssh://[1:2:3:4:5:6:7:8:9]/x"}},
	{"checkRepoURL", []string{"https://[::1]:/x"}},
	{"checkRepoURL", []string{"https://[v1.x]/x"}},
	{"checkRepoURL", []string{"https://h<>/x"}},
	{"checkRepoURL", []string{"https://h\"/x"}},
	{"checkRepoURL", []string{"https://h|/x"}},
	{"checkRepoURL", []string{"https://h{/x"}},
	{"checkRepoURL", []string{"https://h%25/x"}},
	{"checkRepoURL", []string{"https://h%/x"}},
	{"checkRepoURL", []string{"https://h:8%30/x"}},
	{"checkRepoURL", []string{"git://h/x"}},
	{"checkRepoURL", []string{"git://u@h/x"}},
	{"checkRepoURL", []string{"file:///a?b"}},
	{"checkRepoURL", []string{"file://"}},
	{"checkRepoURL", []string{"file:///a#"}},
	{"checkRepoURL", []string{"http://h/a%2Fb"}},
	{"checkRepoURL", []string{"ssh://h.example/x"}},
	{"checkRepoURL", []string{"ssh://h_x~y/x"}},
	{"checkRepoURL", []string{"ssh://H/x"}},
	{"checkRepoURL", []string{"ssh://git@host:65535/r"}},
	{"checkRepoURL", []string{"ssh://git@host:+22/r"}},
	{"checkRepoURL", []string{"git@host:a b"}},
	{"checkRepoURL", []string{"git@h:\u00a0"}},
	{"checkRepoURL", []string{"x@y:z"}},
	{"checkRepoURL", []string{"-x@y:z"}},
	{"checkRepoURL", []string{"x@-y:z"}},
	{"checkRepoURL", []string{"x@y-:z"}},
	{"checkRepoURL", []string{"x.@y:z"}},
	{"checkRepoURL", []string{"x@y.:/z"}},
	{"checkRepoURL", []string{"https://[fe80::1%25%65th0]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25%2]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25a%20b]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25%C3%A9]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25\u00e9]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25a$b]/x"}},
	{"checkRepoURL", []string{"https://[fe80::1%25a/b]/x"}},
	{"checkRepoURL", []string{"https://[%3A%3A1]/x"}},
	// Project names compare as store.ProjectNameKey does (simple case
	// folding, like strings.EqualFold), after trimming.
	{"sameProjectName", []string{"web", "WEB"}},
	{"sameProjectName", []string{" web ", "Web"}},
	{"sameProjectName", []string{"\u0131", "I"}},
	{"sameProjectName", []string{"\u0131", "i"}},
	{"sameProjectName", []string{"i", "I"}},
	{"sameProjectName", []string{"\u0130", "i"}},
	{"sameProjectName", []string{"\u0130", "I"}},
	{"sameProjectName", []string{"K", "\u212a"}},
	{"sameProjectName", []string{"k", "\u212a"}},
	{"sameProjectName", []string{"\u00df", "\u1e9e"}},
	{"sameProjectName", []string{"\u00df", "ss"}},
	{"sameProjectName", []string{"\u017f", "s"}},
	{"sameProjectName", []string{"\u017f", "S"}},
	{"sameProjectName", []string{"\u03a3", "\u03c2"}},
	{"sameProjectName", []string{"\u03c3", "\u03c2"}},
	{"sameProjectName", []string{"caf\u00e9", "CAF\u00c9"}},
	{"sameProjectName", []string{"cafe", "caf\u00e9"}},
	{"sameProjectName", []string{"\u01c5", "\u01c6"}},
	{"sameProjectName", []string{"\u01c5", "\u01c4"}},
	{"sameProjectName", []string{"\ufb00", "ff"}},
	{"sameProjectName", []string{"\u03a9", "\u2126"}},
	{"sameProjectName", []string{"\u00c5", "\u212b"}},
	{"sameProjectName", []string{"\u00b5", "\u03bc"}},
	{"sameProjectName", []string{"\U00010400", "\U00010428"}},
	{"sameProjectName", []string{"a\u0301", "\u00e1"}},
	{"sameProjectName", []string{"\u1e9b", "\u1e61"}},
	{"sameProjectName", []string{"\u0345", "\u03b9"}},
	{"sameProjectName", []string{"\u0345", "\u1fbe"}},
	{"sameProjectName", []string{"\u0398", "\u03d1"}},
	{"sameProjectName", []string{"\u0398", "\u03f4"}},
	{"sameProjectName", []string{"\u10a0", "\u2d00"}},
	{"sameProjectName", []string{"\u13a0", "\uab70"}},
	{"sameProjectName", []string{"\u00ff", "\u0178"}},
	{"sameProjectName", []string{"a", "ab"}},
}

type webParityCase struct {
	Fn     string   `json:"fn"`
	Args   []string `json:"args"`
	Result string   `json:"result"`
	// Empty when accepted; absent for normalizePath and sameProjectName.
	Problem *string `json:"problem,omitempty"`
}

func runWebParityCase(t *testing.T, fn string, args []string) webParityCase {
	t.Helper()
	c := webParityCase{Fn: fn, Args: args}
	arg := func(n int) []string {
		if len(args) != n {
			t.Fatalf("%s takes %d arguments, got %q", fn, n, args)
		}
		return args
	}
	checked := func(result, problem string) { c.Result, c.Problem = result, &problem }
	switch fn {
	case "checkName":
		checked(checkName(arg(1)[0]))
	case "checkPath":
		a := arg(2)
		checked(checkPath(a[0], a[1]))
	case "checkRepoURL":
		checked(checkRepoURL(arg(1)[0]))
	case "normalizePath":
		a := arg(2)
		c.Result = normalizePath(a[0], a[1])
	case "sameProjectName":
		a := arg(2)
		c.Result = fmt.Sprint(store.ProjectNameKey(jsTrim(a[0])) == store.ProjectNameKey(jsTrim(a[1])))
	case "checkBaseURL":
		raw := arg(1)[0]
		checked(raw, checkBaseURL(raw))
	case "checkProviderKey":
		raw := arg(1)[0]
		checked(raw, checkKey(raw))
	case "checkEnvName":
		raw := arg(1)[0]
		checked(raw, providers.CheckEnvName(raw))
	default:
		t.Fatalf("unknown function %q", fn)
	}
	return c
}

// webProviderParityInputs are the provider form's rules (providers.go and
// providers.CheckEnvName): the base URL, the API key and custom variable
// names. They go to their own section, "providerCases", because the Web's
// parity test runs every entry of "cases" through a port it already has;
// the Web wires these once its provider form ports the rules. Each base
// URL case's expected outcome is also pinned in TestCheckBaseURL.
var webProviderParityInputs = func() []webParityInput {
	var out []webParityInput
	for _, c := range baseURLCases {
		out = append(out, webParityInput{"checkBaseURL", []string{c.raw}})
	}
	for _, k := range []string{"sk-fake", "with space inside", "\u00e9t\u00e9", "line\nbreak", "cr\r", "tab\t", "nul\x00", "del\x7f", "c1\u0085"} {
		out = append(out, webParityInput{"checkProviderKey", []string{k}})
	}
	for _, n := range []string{"FOO", "_X1", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CONFIG_DIR", "HOME", "PATH",
		"CIRCUS_X", "CIRCUS", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "lower", "1A", ""} {
		out = append(out, webParityInput{"checkEnvName", []string{n}})
	}
	return out
}()

func webParityJSON(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	about, _ := json.Marshal("What the control plane's rules (internal/controlplane/webapi/rules.go) return " +
		"for the inputs of the Web UI's parity test (validation-parity.test.ts). Generated; do not edit. " +
		"Regenerate with: go test ./internal/controlplane/webapi -run TestWebParityFixture -update")
	b.WriteString("{\n  \"about\": " + string(about) + ",\n")
	seen := map[string]bool{}
	section := func(name string, inputs []webParityInput, last bool) {
		b.WriteString("  \"" + name + "\": [\n")
		for i, in := range inputs {
			key := in.fn + "\x00" + strings.Join(in.args, "\x00")
			if seen[key] {
				t.Fatalf("duplicate parity input %s%q", in.fn, in.args)
			}
			seen[key] = true
			line, err := json.Marshal(runWebParityCase(t, in.fn, in.args))
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString("    ")
			b.Write(line)
			if i < len(inputs)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		if last {
			b.WriteString("  ]\n")
		} else {
			b.WriteString("  ],\n")
		}
	}
	section("cases", webParityInputs, false)
	section("providerCases", webProviderParityInputs, true)
	b.WriteString("}\n")
	return b.Bytes()
}

// caseFoldJSON lists every simple case-folding orbit (unicode.SimpleFold)
// with more than one member, each as one string of its runes in code point
// order. Two names are the same project name exactly when, rune by rune,
// the runes are equal or share an orbit.
func caseFoldJSON(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	about, _ := json.Marshal("Unicode simple case-folding orbits, as Go's unicode.SimpleFold has them " +
		"(Unicode " + unicode.Version + "): project names compare equal ignoring case exactly when their " +
		"code points are pairwise equal or in the same orbit (store.ProjectNameKey). Generated; do not edit. " +
		"Regenerate with: go test ./internal/controlplane/webapi -run TestWebParityFixture -update")
	b.WriteString("{\n  \"about\": " + string(about) + ",\n  \"orbits\": [\n")
	first := true
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) || unicode.SimpleFold(r) == r {
			continue
		}
		orbit := []rune{r}
		smallest := true
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < r {
				smallest = false
				break
			}
			orbit = append(orbit, f)
		}
		if !smallest {
			continue
		}
		// SimpleFold walks up from r and wraps around once; sort the rest.
		for i := 1; i < len(orbit); i++ {
			for j := i; j > 0 && orbit[j] < orbit[j-1]; j-- {
				orbit[j], orbit[j-1] = orbit[j-1], orbit[j]
			}
		}
		s, _ := json.Marshal(string(orbit))
		if !first {
			b.WriteString(",\n")
		}
		first = false
		b.WriteString("    ")
		b.Write(s)
	}
	b.WriteString("\n  ]\n}\n")
	return b.Bytes()
}

func TestWebParityFixture(t *testing.T) {
	for _, f := range []struct {
		path string
		want []byte
	}{
		{webParityFixture, webParityJSON(t)},
		{webCaseFoldTable, caseFoldJSON(t)},
	} {
		if *updateWebFixtures {
			if err := os.WriteFile(f.path, f.want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, f.want) {
			continue
		}
		gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(f.want), "\n")
		for i := 0; i < max(len(gotLines), len(wantLines)); i++ {
			var g, w string
			if i < len(gotLines) {
				g = gotLines[i]
			}
			if i < len(wantLines) {
				w = wantLines[i]
			}
			if g != w {
				t.Errorf("%s line %d is out of date:\n  file: %.300s\n  now:  %.300s", f.path, i+1, g, w)
				break
			}
		}
		t.Errorf("%s differs from what the rules return now; regenerate it with -update", f.path)
	}
}

// TestProjectNameKeyMatchesWebCaseFold checks that store.ProjectNameKey,
// which decides on the server whether two project names are the same, puts
// two runes together exactly when web/src/api/case-fold.json, which the Web
// UI compares names with, has them in the same orbit.
func TestProjectNameKeyMatchesWebCaseFold(t *testing.T) {
	data, err := os.ReadFile(webCaseFoldTable)
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Orbits []string `json:"orbits"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, orbit := range table.Orbits {
		want[orbit] = true
	}

	// Every rune by its key; runes sharing a key are one group.
	groups := map[string][]rune{}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		key := store.ProjectNameKey(string(r))
		groups[key] = append(groups[key], r) // ascending, as r counts up
	}
	got := map[string]bool{}
	for _, runes := range groups {
		if len(runes) > 1 {
			got[string(runes)] = true
		}
	}
	for orbit := range want {
		if !got[orbit] {
			t.Errorf("case-fold.json has the orbit %q (%U), but ProjectNameKey doesn't give its runes one key", orbit, []rune(orbit))
		}
	}
	for group := range got {
		if !want[group] {
			t.Errorf("ProjectNameKey gives %q (%U) one key, but case-fold.json has no such orbit", group, []rune(group))
		}
	}
	if len(want) == 0 {
		t.Fatal("case-fold.json has no orbits")
	}
}
