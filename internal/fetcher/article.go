package fetcher

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hi20160616/exhtml"
	"github.com/hi20160616/gears"
	"github.com/hi20160616/ms-rfa/configs"
	"github.com/pkg/errors"
	"golang.org/x/net/html"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Article struct {
	Id            string
	Title         string
	Content       string
	WebsiteId     string
	WebsiteDomain string
	WebsiteTitle  string
	UpdateTime    *timestamppb.Timestamp
	U             *url.URL
	raw           []byte
	doc           *html.Node
}

func NewArticle() *Article {
	return &Article{
		WebsiteDomain: configs.Data.MS["rfa"].Domain,
		WebsiteTitle:  configs.Data.MS["rfa"].Title,
		WebsiteId:     fmt.Sprintf("%x", md5.Sum([]byte(configs.Data.MS["rfa"].Domain))),
	}
}

// List get all articles from database
func (a *Article) List() ([]*Article, error) {
	return load()
}

// Get read database and return the data by rawurl.
func (a *Article) Get(id string) (*Article, error) {
	as, err := load()
	if err != nil {
		return nil, err
	}

	for _, a := range as {
		if a.Id == id {
			return a, nil
		}
	}
	return nil, fmt.Errorf("[%s] no article with id: %s",
		configs.Data.MS["rfa"].Title, id)

}

func (a *Article) Search(keyword ...string) ([]*Article, error) {
	as, err := load()
	if err != nil {
		return nil, err
	}

	as2 := []*Article{}
	for _, a := range as {
		for _, v := range keyword {
			v = strings.ToLower(strings.TrimSpace(v))
			switch {
			case a.Id == v:
				as2 = append(as2, a)
			case a.WebsiteId == v:
				as2 = append(as2, a)
			case strings.Contains(strings.ToLower(a.Title), v):
				as2 = append(as2, a)
			case strings.Contains(strings.ToLower(a.Content), v):
				as2 = append(as2, a)
			case strings.Contains(strings.ToLower(a.WebsiteDomain), v):
				as2 = append(as2, a)
			case strings.Contains(strings.ToLower(a.WebsiteTitle), v):
				as2 = append(as2, a)
			}
		}
	}
	return as2, nil
}

type ByUpdateTime []*Article

func (u ByUpdateTime) Len() int      { return len(u) }
func (u ByUpdateTime) Swap(i, j int) { u[i], u[j] = u[j], u[i] }
func (u ByUpdateTime) Less(i, j int) bool {
	return u[i].UpdateTime.AsTime().Before(u[j].UpdateTime.AsTime())
}

var timeout = func() time.Duration {
	t, err := time.ParseDuration(configs.Data.MS["rfa"].Timeout)
	if err != nil {
		log.Printf("[%s] timeout init error: %v", configs.Data.MS["rfa"].Title, err)
		return time.Duration(1 * time.Minute)
	}
	return t
}()

// fetchArticle fetch article by rawurl
func (a *Article) fetchArticle(rawurl string) (*Article, error) {
	var err error
	a.U, err = url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	// Dail
	a.raw, a.doc, err = exhtml.GetRawAndDoc(a.U, timeout)
	if err != nil {
		return nil, err
	}

	a.Id = fmt.Sprintf("%x", md5.Sum([]byte(rawurl)))

	a.Title, err = a.fetchTitle()
	if err != nil {
		return nil, err
	}

	a.UpdateTime, err = a.fetchUpdateTime()
	if err != nil {
		return nil, err
	}

	// content should be the last step to fetch
	a.Content, err = a.fetchContent()
	if err != nil {
		return nil, err
	}

	a.Content, err = a.fmtContent(a.Content)
	if err != nil {
		return nil, err
	}
	return a, nil

}

func (a *Article) fetchTitle() (string, error) {
	n := exhtml.ElementsByTag(a.doc, "title")
	if n == nil || len(n) == 0 {
		return "", fmt.Errorf("[%s] there is no element <title>: %s",
			configs.Data.MS["rfa"].Title, a.U.String())
	}
	title := n[0].FirstChild.Data
	title = strings.ReplaceAll(title, " — 普通话主页", "")
	title = strings.TrimSpace(title)
	gears.ReplaceIllegalChar(&title)
	return title, nil
}

func (a *Article) fetchUpdateTime() (*timestamppb.Timestamp, error) {
	if a.doc == nil {
		return nil, errors.Errorf("[%s] fetchUpdateTime: doc is nil: %s",
			configs.Data.MS["rfa"].Title, a.U.String())
	}
	doc := exhtml.ElementsByTagAndType(a.doc, "script", "application/ld+json")
	if doc == nil {
		return nil, fmt.Errorf("[%s] fetchUpdateTime: cannot get target nodes: %s",
			configs.Data.MS["rfa"].Title, a.U.String())
	}
	d := doc[0].FirstChild
	if d.Type != html.TextNode {
		return nil, fmt.Errorf("[%s] fetchUpdateTime: target node have no text: %s",
			configs.Data.MS["rfa"].Title, a.U.String())
	}
	raw := d.Data
	re := regexp.MustCompile(`"date\w*?":\s*?"(.*?)"`)
	rs := re.FindAllStringSubmatch(raw, -1)
	// dateModified -> rs[0][1], datePublished -> rs[1][1]
	t, err := time.Parse(time.RFC3339, rs[0][1])
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

func shanghai(t time.Time) time.Time {
	loc := time.FixedZone("UTC", 8*60*60)
	return t.In(loc)
}

func (a *Article) fetchContent() (string, error) {
	if a.raw == nil {
		return "", errors.Errorf("[%s] fetchContent: raw is nil: %s", configs.Data.MS["rfa"].Title, a.U.String())
	}
	body := ""
	// Fetch content nodes
	var ps [][]byte
	var b bytes.Buffer
	var re = regexp.MustCompile(`(?m)<p.*?>(?P<content>.*?)</p>`)
	for _, v := range re.FindAllSubmatch(a.raw, -1) {
		ps = append(ps, v[1])
	}
	if len(ps) == 0 {
		if regexp.MustCompile(`(?m)<video.*?>`).FindAll(a.raw, -1) != nil {
			return "", errors.New("\n[-] fetcher.FmtBodyRfa() Error: this is a video page.\n")
		}
		return "", errors.New("\n[-] fetcher.FmtBodyRfa() Error: regex matched nothing.\n")
	} else {
		for _, p := range ps {
			b.Write(p)
			b.Write([]byte("  \n"))
		}
	}
	replace := func(src, x, y string) string {
		re := regexp.MustCompile(x)
		return re.ReplaceAllString(src, y)
	}
	body = html.UnescapeString(string(b.Bytes()))
	body = replace(body, `(?m)<i.*?</i>`, "")
	body = replace(body, `(?m)<iframe.*?</iframe>`, "")
	body = replace(body, `(?m)<iframe.*?</iframe>`, "")
	rp := strings.NewReplacer("\n\n", "\n",
		"<br/>", "",
		"<b>", "**", "</b>", "**  \n",
		"<strong>", "**", "</strong>", "**  \n",
		"****", "",
		"** **", "")
	body = rp.Replace(body)
	return body, nil
}

func (a *Article) fmtContent(body string) (string, error) {
	var err error
	title := "# " + a.Title + "\n\n"
	lastupdate := shanghai(a.UpdateTime.AsTime()).Format(time.RFC3339)
	webTitle := fmt.Sprintf(" @ [%s](/list/?v=%[1]s): [%[2]s](http://%[2]s)", a.WebsiteTitle, a.WebsiteDomain)
	u, err := url.QueryUnescape(a.U.String())
	if err != nil {
		u = a.U.String() + "\n\nunescape url error:\n" + err.Error()
	}

	body = title +
		"LastUpdate: " + lastupdate +
		webTitle + "\n\n" +
		"---\n" +
		body + "\n\n" +
		"原地址：" + fmt.Sprintf("[%s](%[1]s)", u)
	return body, nil
}
