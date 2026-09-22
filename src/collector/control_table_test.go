package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlTableLayoutAndOps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phpray-control")
	ct, err := OpenControlTable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	info, _ := os.Stat(path)
	if info.Size() != ctrlFileSize || ctrlFileSize != 64+256*160 {
		t.Fatalf("file size %d, want %d", info.Size(), ctrlFileSize)
	}
	raw, _ := os.ReadFile(path)
	if binary.LittleEndian.Uint32(raw[0:4]) != 0x50485243 || binary.LittleEndian.Uint32(raw[4:8]) != 1 || binary.LittleEndian.Uint32(raw[16:20]) != 256 {
		t.Fatalf("header: %x", raw[:24])
	}
	if ct.Seq()%2 != 0 {
		t.Fatalf("seq must be even when idle: %d", ct.Seq())
	}
	// FNV-1a 64 reference values (must match phpray_ctrl_hash in control.c)
	if FNVHash64("") != 14695981039346656037 || FNVHash64("a") != 0xaf63dc4c8601ec8c {
		t.Fatalf("fnv: %x %x", FNVHash64(""), FNVHash64("a"))
	}

	until := time.Now().Add(10 * time.Minute).Unix()
	if err := ct.Set("/var/www/shop", "/checkout/", 100, until); err != nil {
		t.Fatal(err)
	}
	if err := ct.Set("/var/www/blog", "", 25, until); err != nil {
		t.Fatal(err)
	}
	list := ct.List()
	if len(list) != 2 || list[0].DocrootHash != FNVHash64("/var/www/shop") || list[0].URLPrefix != "/checkout/" || list[0].SampleRate != 100 || !list[0].Active {
		t.Fatalf("list: %+v", list)
	}
	// raw entry layout: hash u64, until u64, rate u16, flags u16, reserved u32, prefix[136]
	raw, _ = os.ReadFile(path)
	e := raw[64 : 64+160]
	if binary.LittleEndian.Uint64(e[0:8]) != FNVHash64("/var/www/shop") || int64(binary.LittleEndian.Uint64(e[8:16])) != until ||
		binary.LittleEndian.Uint16(e[16:18]) != 100 || binary.LittleEndian.Uint16(e[18:20]) != 1 || string(e[24:34]) != "/checkout/" || e[34] != 0 {
		t.Fatalf("entry layout: %x", e[:40])
	}
	// replace keeps one slot per docroot
	if err := ct.Set("/var/www/shop", "/cart/", 50, until); err != nil {
		t.Fatal(err)
	}
	if l := ct.List(); len(l) != 2 || l[0].URLPrefix != "/cart/" || l[0].SampleRate != 50 {
		t.Fatalf("replace: %+v", l)
	}
	// seq advanced by 2 per write (3 writes so far)
	if ct.Seq() != 6 {
		t.Fatalf("seq after 3 writes: %d", ct.Seq())
	}
	if !ct.Clear("/var/www/blog") || len(ct.List()) != 1 {
		t.Fatalf("clear")
	}
	// expired entries are dropped and their slot reused
	if err := ct.Set("/var/www/old", "/x", 100, time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if n := ct.Expire(time.Now().Unix()); n != 1 || len(ct.List()) != 1 {
		t.Fatalf("expire: n=%d list=%+v", n, ct.List())
	}
	if err := ct.Set("/var/www/new", "/y", 100, until); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(func() []byte { b, _ := os.ReadFile(path); return b }()[12:16]) != 2 {
		t.Fatalf("expired slot should be reused (entry_count 2)")
	}
	// reopen keeps the data
	ct2, err := OpenControlTable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ct2.Close()
	if l := ct2.List(); len(l) != 2 || l[0].URLPrefix != "/cart/" {
		t.Fatalf("reopen: %+v", l)
	}
}

func TestControlProfileMessageWritesTable(t *testing.T) {
	dir := t.TempDir()
	ct, err := OpenControlTable(filepath.Join(dir, "ctl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	s := newTestShipper(t, "http://127.0.0.1:9", func(c *CloudConfig) { c.BufferDir = filepath.Join(dir, "buf") })
	s.control = ct
	tr := sampleTrace(time.Now().Unix(), "shop.example", "/", "summary", 10)
	s.Observe(tr) // learns site_id → docroot
	site := SiteID("shop.example", "/var/www/shop.example")
	s.applyControl(cloudControlMessage{ID: "m_9", Type: "profile", SiteID: site, URLPrefix: "/checkout/", SampleRate: 100, Until: time.Now().Unix() + 300})
	l := ct.List()
	if len(l) != 1 || l[0].DocrootHash != FNVHash64("/var/www/shop.example") || l[0].URLPrefix != "/checkout/" {
		t.Fatalf("control table after profile message: %+v", l)
	}
	// unknown site: ignored, table unchanged
	s.applyControl(cloudControlMessage{ID: "m_10", Type: "profile", SiteID: "nope", URLPrefix: "/x", SampleRate: 100, Until: time.Now().Unix() + 300})
	if len(ct.List()) != 1 {
		t.Fatalf("unknown site must not add entries")
	}
}
