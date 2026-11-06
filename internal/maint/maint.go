// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package maint selects a git tree for a set of changed files based on
// a DPDK-style MAINTAINERS file.
package maint

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

type pattern struct {
	re      *regexp.Regexp
	glob    string
	section int
	block   int
}

type block struct {
	tree string
}

type section struct {
	tree string
}

type File struct {
	patterns []pattern
	blocks   []block
	sections []section
}

var dashesRe = regexp.MustCompile(`^-{3,}\s*$`)

// Parse reads a MAINTAINERS file.  Sections start with a title line
// followed by a line of dashes; blocks within a section are separated
// by blank lines.
func Parse(data string) *File {
	f := &File{}
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	cur := -1
	blk := -1
	firstPara, content := false, false
	newBlock := func() {
		f.blocks = append(f.blocks, block{})
		blk = len(f.blocks) - 1
	}
	newBlock()
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		if i+1 < len(lines) && line != "" && dashesRe.MatchString(lines[i+1]) {
			f.sections = append(f.sections, section{})
			cur = len(f.sections) - 1
			firstPara, content = true, false
			newBlock()
			i++
			continue
		}
		if line == "" {
			if content {
				firstPara = false
			}
			newBlock()
			continue
		}
		content = true
		key, val, ok := strings.Cut(line, ":")
		if !ok || len(key) != 1 {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "T":
			name := treeName(val)
			if f.blocks[blk].tree == "" {
				f.blocks[blk].tree = name
			}
			if cur >= 0 && firstPara && f.sections[cur].tree == "" {
				f.sections[cur].tree = name
			}
		case "F":
			glob := val
			if strings.HasSuffix(glob, "/") {
				glob += "*"
			}
			f.patterns = append(f.patterns, pattern{
				re: globRegexp(glob), glob: glob, section: cur, block: blk,
			})
		}
	}
	return f
}

func treeName(v string) string {
	v = strings.TrimSpace(strings.Fields(v + " ")[0])
	return path.Base(strings.TrimSuffix(strings.TrimSuffix(v, "/"), ".git"))
}

// globRegexp converts a shell glob where '*' also matches '/'.
func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			if j := strings.IndexByte(glob[i:], ']'); j > 0 {
				b.WriteString(glob[i : i+j+1])
				i += j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(`^` + regexp.QuoteMeta(glob) + `$`)
	}
	return re
}

// TreeFor returns the tree of the first matching pattern, or "" if the
// file does not belong to a tree.
func (f *File) TreeFor(file string) string {
	for _, p := range f.patterns {
		if strings.Contains(p.glob, "doc/*") || !p.re.MatchString(file) {
			continue
		}
		if t := f.blocks[p.block].tree; t != "" {
			return t
		}
		if p.section >= 0 {
			return f.sections[p.section].tree
		}
		return ""
	}
	return ""
}

const virtio, virtioAlias = "dpdk-next-virtio", "dpdk-next-net-virtio"

// Select returns the tree for a set of files, or "" for the main tree.
func (f *File) Select(files []string) string {
	byFile := map[string]string{}
	for _, file := range files {
		if t := f.TreeFor(file); t != "" {
			byFile[file] = t
		}
	}
	tree := commonTree(byFile)
	if tree == "dpdk-next" {
		for file := range byFile {
			if strings.HasPrefix(file, "drivers/common/") {
				delete(byFile, file)
			}
		}
		set := uniqueTrees(byFile)
		if len(set) == 1 {
			return set[0]
		}
		return ""
	}
	return tree
}

func uniqueTrees(byFile map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range byFile {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func commonTree(byFile map[string]string) string {
	trees := uniqueTrees(byFile)
	if len(trees) == 0 {
		return ""
	}
	if len(trees) == 1 {
		return trees[0]
	}
	names := map[string]bool{}
	for i, t := range trees {
		if t == virtio {
			trees[i] = virtioAlias
		}
		names[trees[i]] = true
	}
	prefix := trees[0]
	for _, t := range trees[1:] {
		n := 0
		for n < len(prefix) && n < len(t) && prefix[n] == t[n] {
			n++
		}
		prefix = prefix[:n]
	}
	if !names[prefix] {
		if i := strings.LastIndexByte(prefix, '-'); i >= 0 {
			prefix = prefix[:i]
		} else {
			prefix = ""
		}
	}
	if prefix == virtioAlias {
		prefix = virtio
	}
	return prefix
}

var diffFileRe = regexp.MustCompile(`(?m)^(?:diff --git a/\S+ b/(\S+)|--- a/(\S+)|\+\+\+ b/(\S+))`)

// DiffFiles lists files touched by unified diffs.
func DiffFiles(diffs ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range diffs {
		for _, m := range diffFileRe.FindAllStringSubmatch(d, -1) {
			for _, name := range m[1:] {
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
