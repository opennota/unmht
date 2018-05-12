// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU General
// Public License for more details.
//
// You should have received a copy of the GNU General Public License along
// with this program.  If not, see <http://www.gnu.org/licenses/>.

// View mht files in a browser.
package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/pkg/browser"
)

type file struct {
	contentType string
	base        *url.URL
	data        []byte
	initial     bool
	converted   bool
}

var (
	timeout = flag.Duration("t", 15*time.Second, "Timeout (default: 15 seconds)")

	rWithProto = regexp.MustCompile("^[a-z]+:")
	rURL       = regexp.MustCompile(`\burl\(([^()]+)\)`)

	files   = make(map[string]*file)
	cid2loc = make(map[string]string)
)

func abs(base *url.URL, url string) string {
	if rWithProto.MatchString(url) {
		return url
	}
	if strings.HasPrefix(url, "//") {
		return base.Scheme + ":" + url
	}
	if strings.HasPrefix(url, "/") {
		return base.Scheme + "://" + base.Host + url
	}
	return base.Scheme + "://" + base.Host + path.Join("/", path.Dir(base.Path), url)
}

func modifyCSS(base *url.URL, data []byte) []byte {
	return rURL.ReplaceAllFunc(data, func(d []byte) []byte {
		u := string(d[4 : len(d)-1])
		u = strings.Trim(u, `"'`)
		if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "mailto:") {
			return d
		}
		if strings.HasPrefix(u, "cid:") {
			cid := strings.TrimPrefix(u, "cid:")
			u = cid2loc[cid]
		}
		u = abs(base, u)
		return []byte("url(/" + url.PathEscape(u) + ")")
	})
}

func modifyHTML(base *url.URL, data []byte, addOnLoad, converted bool) ([]byte, error) {
	d, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	if converted {
		d.Find("meta[http-equiv], meta[charset]").Each(func(_ int, sel *goquery.Selection) {
			httpEq, ok := sel.Attr("http-equiv")
			if ok && strings.ToLower(httpEq) == "content-type" {
				sel.SetAttr("content", "text/html; charset=utf-8")
			} else if !ok {
				sel.SetAttr("charset", "utf-8")
			}
		})
	}

	redefinedBase := d.Find("head > base[href]")
	if redefinedBase.Length() > 0 {
		href, _ := redefinedBase.First().Attr("href")
		u, err := url.Parse(href)
		if err != nil {
			return nil, err
		}
		base = u
		redefinedBase.Remove()
	}

	m := cascadia.MustCompile("a[href]")
	for _, attr := range []string{"src", "href", "background"} {
		d.Find("[" + attr + "]").Each(func(_ int, sel *goquery.Selection) {
			if sel.IsMatcher(m) {
				return
			}
			v, _ := sel.Attr(attr)
			if strings.HasPrefix(v, "data:") || strings.HasPrefix(v, "mailto:") {
				return
			}
			if strings.HasPrefix(v, "cid:") {
				cid := strings.TrimPrefix(v, "cid:")
				v = cid2loc[cid]
			}
			v = abs(base, v)
			sel.SetAttr(attr, "/"+url.PathEscape(v))

			sel.RemoveAttr("integrity")
		})
	}
	d.Find("style").Each(func(_ int, sel *goquery.Selection) {
		style := sel.Text()
		if !strings.Contains(style, "url(") {
			return
		}
		style = string(modifyCSS(base, []byte(style)))
		sel.SetText(style)
	})
	d.Find("[style]").Each(func(_ int, sel *goquery.Selection) {
		style, _ := sel.Attr("style")
		if !strings.Contains(style, "url(") {
			return
		}
		style = string(modifyCSS(base, []byte(style)))
		sel.SetAttr("style", style)
	})

	if addOnLoad {
		d.Find("head").First().AppendHtml(`<script>
window.onload = function() {
  var req = new XMLHttpRequest();
  req.open('GET', '/done-signal');
  req.send();
};
</script>`)
	}

	html, err := d.Html()
	if err != nil {
		return nil, err
	}
	return []byte(html), nil
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/done-signal" {
		os.Exit(0)
	}

	url := strings.TrimPrefix(r.URL.Path, "/")
	file, ok := files[url]
	if !ok {
		found := false
		lower := strings.ToLower(url)
		for k, f := range files {
			if strings.ToLower(k) == lower {
				found = true
				file = f
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Add("Content-Type", file.contentType)
	w.Header().Add("Content-Length", fmt.Sprint(len(file.data)))
	w.Write(file.data)
}

func main() {
	log.SetFlags(log.Lshortfile)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "USAGE: unmht [-t TIMEOUT] FILE")
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(0)
	}

	f, err := os.Open(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	msg, err := mail.ReadMessage(f)
	if err != nil {
		log.Fatal(err)
	}

	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil {
		log.Fatal(err)
	}

	mpr := multipart.NewReader(msg.Body, boundary)
	initialLoc := ""
	for {
		part, err := mpr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		contentLocation := part.Header.Get("Content-Location")
		base, err := url.Parse(contentLocation)
		if err != nil {
			log.Fatal(err)
		}

		data, err := ioutil.ReadAll(part)
		if err != nil {
			log.Fatal(err)
		}

		if part.Header.Get("Content-Transfer-Encoding") == "base64" {
			n := base64.StdEncoding.DecodedLen(len(data))
			buf := make([]byte, n)
			n, err := base64.StdEncoding.Decode(buf, data)
			if err != nil {
				log.Fatal(err)
			}
			data = buf[:n]
		}

		if cid := part.Header.Get("Content-ID"); cid != "" {
			cid = strings.Trim(cid, "<>")
			cid2loc[cid] = contentLocation
		}

		contentType := part.Header.Get("Content-Type")
		initial := false
		if initialLoc == "" && (contentType == "text/html" || strings.HasPrefix(contentType, "text/html;")) {
			initialLoc = contentLocation
			initial = true
		}
		files[contentLocation] = &file{contentType, base, data, initial, false}
	}

	if initialLoc == "" {
		log.Fatal("no HTML pages to display")
	}

	for _, file := range files {
		if ct := file.contentType; ct == "text/css" {
			file.data = modifyCSS(file.base, file.data)
		} else if ct == "text/html" || strings.HasPrefix(ct, "text/html;") {
			encoding, name, certain := charset.DetermineEncoding(file.data, file.contentType)
			if name != "utf-8" && !(name == "windows-1252" && !certain) {
				decoded, err := encoding.NewDecoder().Bytes(file.data)
				if err != nil {
					log.Fatal(err)
				}
				file.data = decoded
				if strings.Contains(file.contentType, ";") {
					file.contentType = strings.SplitN(file.contentType, ";", 2)[0]
				}
				file.converted = true
			}
			var err error
			file.data, err = modifyHTML(file.base, file.data, file.initial, file.converted)
			if err != nil {
				log.Fatal(err)
			}
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	initialURL := srv.URL + "/" + url.PathEscape(initialLoc)
	if err := browser.OpenURL(initialURL); err != nil {
		log.Println("Couldn't start browser:", err)
		log.Println("Open the following URL manually:")
		log.Println(initialURL)
	} else {
		timer := time.NewTimer(*timeout)
		go func() {
			<-timer.C
			srv.Close()
			os.Exit(0)
		}()
	}

	select {}
}
