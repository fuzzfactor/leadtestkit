// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package maint

import (
	"strings"
	"testing"
)

const testMaintainers = `Lorem MAINTAINERS file
======================

General Project Administration
------------------------------

Main Branch
M: Lorem Ipsum <lorem@example.org>
T: git://git.example.org/dpdk

Next-net Tree
M: Dolor Sit <dolor@example.org>
T: git://git.example.org/next/dpdk-next-net

Documentation (with overlaps)
F: doc/*
F: README


Core Libraries
--------------
T: git://git.example.org/dpdk

Memory
M: Amet <amet@example.org>
F: lib/eal/
F: doc/guides/prog_guide/env_abstraction_layer.rst

Crypto API
M: Consectetur <cons@example.org>
T: git://git.example.org/next/dpdk-next-crypto
F: lib/cryptodev/


Networking Drivers
------------------
M: Dolor Sit <dolor@example.org>
T: git://git.example.org/next/dpdk-next-net

Lorem bonding
F: drivers/net/bonding/

Intel lorem
T: git://git.example.org/next/dpdk-next-net-intel
F: drivers/net/intel/lorem/
F: drivers/common/lorem*

Mellanox ipsum
T: git://git.example.org/next/dpdk-next-net-mlx
F: drivers/net/mlx5/
F: drivers/common/mlx5/

Virtio
T: git://git.example.org/next/dpdk-next-virtio
F: drivers/net/virtio/

Event Drivers
-------------
T: git://git.example.org/next/dpdk-next-eventdev

Lorem event
F: drivers/event/lorem/
`

func TestTreeFor(t *testing.T) {
	f := Parse(testMaintainers)
	tests := map[string]string{
		"lib/eal/common/eal.c":                            "dpdk",
		"lib/cryptodev/rte_crypto.h":                      "dpdk-next-crypto",
		"drivers/net/bonding/bond.c":                      "dpdk-next-net",
		"drivers/net/intel/lorem/x.c":                     "dpdk-next-net-intel",
		"drivers/common/lorem_ipsum/y.c":                  "dpdk-next-net-intel",
		"drivers/net/mlx5/mlx5.c":                         "dpdk-next-net-mlx",
		"drivers/event/lorem/ev.c":                        "dpdk-next-eventdev",
		"doc/guides/rel_notes/x.rst":                      "",
		"README":                                          "dpdk",
		"unknown/file.c":                                  "",
		"doc/guides/prog_guide/env_abstraction_layer.rst": "dpdk",
	}
	for file, want := range tests {
		if got := f.TreeFor(file); got != want {
			t.Errorf("TreeFor(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestSelect(t *testing.T) {
	f := Parse(testMaintainers)
	tests := []struct {
		files []string
		want  string
	}{
		{[]string{"drivers/net/intel/lorem/x.c", "doc/guides/rel_notes/x.rst"}, "dpdk-next-net-intel"},
		{[]string{"drivers/net/intel/lorem/x.c", "drivers/net/mlx5/m.c"}, "dpdk-next-net"},
		{[]string{"drivers/net/virtio/v.c", "drivers/net/bonding/b.c"}, "dpdk-next-net"},
		{[]string{"drivers/net/virtio/v.c"}, "dpdk-next-virtio"},
		{[]string{"lib/cryptodev/c.c", "drivers/net/virtio/v.c"}, ""},
		{[]string{"lib/eal/e.c", "drivers/net/bonding/b.c"}, "dpdk"},
		{[]string{"drivers/net/mlx5/m.c", "drivers/common/lorem_x/c.c", "lib/cryptodev/c.c"}, ""},
		{[]string{"drivers/event/lorem/e.c", "drivers/common/mlx5/c.c"}, "dpdk-next-eventdev"},
		{[]string{"doc/guides/rel_notes/x.rst"}, ""},
	}
	for _, tt := range tests {
		if got := f.Select(tt.files); got != tt.want {
			t.Errorf("Select(%v) = %q, want %q", tt.files, got, tt.want)
		}
	}
}

func TestDiffFiles(t *testing.T) {
	diff := `diff --git a/lib/a.c b/lib/a.c
--- a/lib/a.c
+++ b/lib/a.c
@@ -1 +1 @@
-x
+y
diff --git a/old.c b/old.c
deleted file mode 100644
--- a/old.c
+++ /dev/null
`
	got := strings.Join(DiffFiles(diff, "diff --git a/new.c b/new.c\n--- /dev/null\n+++ b/new.c\n"), ",")
	if got != "lib/a.c,new.c,old.c" {
		t.Errorf("got %q", got)
	}
}
