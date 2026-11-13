// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pw

import (
	"mime"
	"net/mail"
	"strings"
)

var addressParser = mail.AddressParser{WordDecoder: new(mime.WordDecoder)}

// Sender returns the sender of a patch email, whom a reply goes to.
// Mailing lists rewrite From for DMARC to "<name> via <list>" with the
// address of the list, and relays like B4 Relay rewrite it to "<name>
// via B4 Relay"; Patchwork does not always undo that.  The original
// address is then taken from X-Original-From, a Reply-To other than
// the rewritten address, or a Cc of the same name.  ok is false when
// none has it; name is still known then.
func Sender(p *PatchDetail) (name, email string, ok bool) {
	from, err := addressParser.Parse(rawHeader(p.Headers, "From"))
	if err != nil {
		from = &mail.Address{Name: p.Submitter.Name, Address: p.Submitter.Email}
	}
	name, rewritten := unvia(from.Name)
	if !rewritten {
		return from.Name, from.Address, from.Address != ""
	}
	if a, err := addressParser.Parse(rawHeader(p.Headers, "X-Original-From")); err == nil {
		if a.Name != "" {
			name = a.Name
		}
		return name, a.Address, true
	}
	list, _ := addressParser.ParseList(rawHeader(p.Headers, "Reply-To"))
	if len(list) == 1 && !strings.EqualFold(list[0].Address, from.Address) {
		if list[0].Name != "" {
			name = list[0].Name
		}
		return name, list[0].Address, true
	}
	// Mailman moves the original From to Cc when Reply-To points to
	// the list.
	cc, _ := addressParser.ParseList(rawHeader(p.Headers, "Cc"))
	for _, a := range cc {
		if n, _ := unvia(a.Name); name != "" && n == name &&
			!strings.EqualFold(a.Address, from.Address) {
			return name, a.Address, true
		}
	}
	return name, "", false
}

// unvia cuts a name at the first " via ", which rewrites can stack:
// "Lorem via B4 Relay via dev".
func unvia(name string) (string, bool) {
	i := strings.Index(name, " via ")
	if i < 0 {
		if !strings.HasSuffix(name, " via") {
			return name, false
		}
		i = len(name) - len(" via")
	}
	return strings.Trim(strings.TrimSpace(name[:i]), `'"`), true
}
