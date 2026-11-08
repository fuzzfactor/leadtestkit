// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/mailarchive.go (commit 1f9d0e7), modified.

package archive

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"leadtestkit/internal/httpx"
)

type ArchiveMessage struct {
	Number  int
	Subject string
}

// Pipermail is a mailman 2 archive, e.g.
// https://mail.example.org/pipermail/dev/.
type Pipermail struct {
	http *httpx.Client
	base string
}

func NewPipermail(baseURL string, hc *httpx.Client) *Pipermail {
	return &Pipermail{http: hc, base: strings.TrimRight(baseURL, "/") + "/"}
}

// MonthKey is the pipermail directory name, e.g. "2026-March".
func MonthKey(year int, month time.Month) string {
	return fmt.Sprintf("%d-%s", year, month.String())
}

func BuildArchiveURL(baseURL string, year int, month time.Month) string {
	return strings.TrimRight(baseURL, "/") + "/" + MonthKey(year, month) + "/date.html"
}

// MessageURL follows mailman 2, which pads message numbers to six digits.
func (p *Pipermail) MessageURL(month string, number int) string {
	return fmt.Sprintf("%s%s/%06d.html", p.base, month, number)
}

// FetchMonth lists the messages of one month.  A missing month (no
// mail yet) is an empty list, not an error.
func (p *Pipermail) FetchMonth(ctx context.Context, year int,
	month time.Month) ([]ArchiveMessage, error) {
	resp, err := p.http.Get(ctx, BuildArchiveURL(p.base, year, month))
	if httpx.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseArchiveMessages(resp.Body)
}

var archiveMsgRe = regexp.MustCompile(`(?i)<LI><A HREF="(\d+)\.html">([^<]+)`)

func parseArchiveMessages(body []byte) ([]ArchiveMessage, error) {
	matches := archiveMsgRe.FindAllSubmatch(body, -1)
	msgs := make([]ArchiveMessage, 0, len(matches))
	for _, m := range matches {
		num, err := strconv.Atoi(string(m[1]))
		if err != nil {
			continue
		}
		msgs = append(msgs, ArchiveMessage{Number: num,
			Subject: cleanArchiveSubject(string(m[2]))})
	}
	return msgs, nil
}

func cleanArchiveSubject(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return strings.TrimSpace(s)
	}
	return strings.Join(strings.Fields(textContent(doc)), " ")
}

type Message struct {
	URL       string
	Subject   string
	MessageID string
	Date      string
	// Body is the plain text of the article.  Links holds every href
	// found in it, since pipermail turns URLs into anchors.
	Body  string
	Links []string
}

func (p *Pipermail) FetchMessage(ctx context.Context, msgURL string) (*Message, error) {
	resp, err := p.http.Get(ctx, msgURL)
	if err != nil {
		return nil, err
	}
	m, err := ParseMessagePage(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot parse %s because %w", msgURL, err)
	}
	m.URL = msgURL
	return m, nil
}

var (
	h1Re         = regexp.MustCompile(`(?is)<H1>(.*?)</H1>`)
	inReplyToRe  = regexp.MustCompile(`(?i)[?&]In-Reply-To=([^"&>\s]+)`)
	dateRe       = regexp.MustCompile(`(?is)<I>([^<]*)</I>`)
	articleRe    = regexp.MustCompile(`(?is)<!--beginarticle-->(.*?)<!--endarticle-->`)
	hrefRe       = regexp.MustCompile(`(?i)href="([^"]+)"`)
	beginArticle = "<!--beginarticle-->"
)

// ParseMessagePage parses a pipermail article page.  The mailto reply
// link carries the message's own Message-ID as In-Reply-To.
func ParseMessagePage(page []byte) (*Message, error) {
	art := articleRe.FindSubmatch(page)
	if art == nil {
		return nil, fmt.Errorf("no article body")
	}
	head := string(page)
	if i := strings.Index(head, beginArticle); i >= 0 {
		head = head[:i]
	}
	m := &Message{}
	if h := h1Re.FindStringSubmatch(head); h != nil {
		m.Subject = cleanArchiveSubject(h[1])
	}
	if r := inReplyToRe.FindStringSubmatch(head); r != nil {
		if id, err := url.QueryUnescape(r[1]); err == nil {
			m.MessageID = strings.TrimSpace(id)
		}
	}
	if d := dateRe.FindStringSubmatch(head); d != nil {
		m.Date = strings.TrimSpace(html.UnescapeString(d[1]))
	}
	for _, h := range hrefRe.FindAllSubmatch(art[1], -1) {
		m.Links = append(m.Links, html.UnescapeString(string(h[1])))
	}
	doc, err := html.Parse(strings.NewReader("<pre>" + string(art[1])))
	if err != nil {
		return nil, err
	}
	m.Body = strings.TrimLeft(textContent(doc), "\n")
	return m, nil
}

var bracketPrefixRe = regexp.MustCompile(`^\[[^\]]*\]\s*`)

// Matches "v2", "V10", "PATCHv2" etc. — no word boundary before v
// because "PATCHv2" is a common format in the wild.
var versionInBracketRe = regexp.MustCompile(`(?i)v(\d+)\b`)

// extractVersion extracts a version token (e.g., "v2") from a patch
// subject. Skips to the first '[' (ignoring any reply prefix in any
// language), then scans consecutive bracket groups. Stops as soon as
// non-bracket text is encountered — brackets buried in (was: ...) or
// quoted text are ignored, and the caller falls back to matching all
// versions to avoid false negatives.
func extractVersion(subject string) string {
	s := strings.TrimSpace(subject)
	i := strings.Index(s, "[")
	if i < 0 {
		return ""
	}
	// A '(' before the first '[' means the bracket is likely inside a
	// parenthetical like "(was: [PATCH v2] ...)".
	if strings.Contains(s[:i], "(") {
		return ""
	}
	s = s[i:]
	for strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			break
		}
		if m := versionInBracketRe.FindString(s[1:end]); m != "" {
			return strings.ToLower(m)
		}
		s = strings.TrimSpace(s[end+1:])
	}
	return ""
}

// versionsMatch treats unversioned patches as v1.
func versionsMatch(msgVersion, patchVersion string) bool {
	if msgVersion == patchVersion {
		return true
	}
	if msgVersion == "v1" && patchVersion == "" {
		return true
	}
	return patchVersion == "v1" && msgVersion == ""
}

func ExtractPatchCore(s string) string {
	for {
		stripped := bracketPrefixRe.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = stripped
	}
	return strings.TrimSpace(s)
}

const lcsMatchThreshold = 80

func subjectMatch(patchCore, emailSubject string) bool {
	pc := strings.ToLower(patchCore)
	es := strings.ToLower(emailSubject)
	if pc == "" {
		return false
	}
	if strings.Contains(es, pc) {
		return true
	}
	lcs := longestCommonSubstring(pc, es)
	return lcs*100/len(pc) >= lcsMatchThreshold
}

func longestCommonSubstring(a, b string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	maxLen := 0
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
				if curr[j] > maxLen {
					maxLen = curr[j]
				}
			} else {
				curr[j] = 0
			}
		}
		prev, curr = curr, prev
		for k := range curr {
			curr[k] = 0
		}
	}
	return maxLen
}

func MatchPatchSubjects(msgs []ArchiveMessage, patchNames map[int]string) []int {
	type patchInfo struct {
		core    string
		version string
	}
	patches := map[int]patchInfo{}
	for id, name := range patchNames {
		if c := ExtractPatchCore(name); c != "" {
			patches[id] = patchInfo{core: c, version: extractVersion(name)}
		}
	}

	matched := map[int]bool{}
	for _, msg := range msgs {
		msgVersion := extractVersion(msg.Subject)
		for id, p := range patches {
			if !subjectMatch(p.core, msg.Subject) {
				continue
			}
			// Without an extractable version, match all versions to
			// avoid false negatives.
			if msgVersion == "" || versionsMatch(msgVersion, p.version) {
				matched[id] = true
			}
		}
	}

	ids := make([]int, 0, len(matched))
	for id := range matched {
		ids = append(ids, id)
	}
	return ids
}

func FilterNewMessages(msgs []ArchiveMessage, lastSeen int) []ArchiveMessage {
	var result []ArchiveMessage
	for _, m := range msgs {
		if m.Number > lastSeen {
			result = append(result, m)
		}
	}
	return result
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textContent(c))
	}
	return b.String()
}
