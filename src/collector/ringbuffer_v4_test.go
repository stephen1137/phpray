package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildTraceRecord serializes a trace payload in the packed layout of
// phpray_trace_record_t (src/extension/ringbuffer.h) for ring version ver (3 or 4).
func buildTraceRecord(ver uint32, comps []Component, app, profiled uint8) []byte {
	return buildTraceRecordFor(ver, "shop.local", "/sklep/", comps, app, profiled)
}

// buildTraceRecordFor is buildTraceRecord with the host and URI of the request.
func buildTraceRecordFor(ver uint32, host, uri string, comps []Component, app, profiled uint8) []byte {
	var b bytes.Buffer
	w := func(v interface{}) { _ = binary.Write(&b, binary.LittleEndian, v) }
	method, id, phpVer := "GET", "pr-1-2-3", "8.3.33"
	docroot := "/var/www/html"

	w(uint64(42))          // request_id
	w(uint32(1000))        // uid
	w(uint32(4242))        // pid
	w(uint64(1758200000))  // timestamp
	w(uint64(400_000_000)) // duration_ns
	w(uint64(300_000_000)) // cpu_user_ns
	w(uint64(50_000_000))  // cpu_sys_ns
	w(uint16(200))         // response_code
	w(uint64(64 << 20))    // memory_peak
	w(uint16(0))           // query_count
	w(uint64(0))           // db_total_ns
	w(uint16(0))           // http_call_count
	w(uint64(0))           // http_total_ns
	w(uint16(0))           // file_op_count
	w(uint64(0))           // file_total_ns
	w(uint16(0))           // redis_call_count (v3+)
	w(uint64(0))           // redis_total_ns
	w(uint8(1))            // wp_detected
	w(uint8(1))            // trace_level (normal)
	w(uint16(0))           // mark_count
	w(uint16(0))           // error_count
	w(uint8(0))            // n_plus_one
	w(uint16(len(host)))
	w(uint16(len(uri)))
	w(uint16(len(method)))
	w(uint16(len(id)))
	w(uint8(len(phpVer)))
	w(uint16(0)) // serialized_query_count
	w(uint16(0)) // serialized_http_count
	w(uint16(0)) // serialized_mark_count
	w(uint16(0)) // serialized_error_count
	w(uint16(len(comps)))
	if ver >= 4 {
		w(app)
		w(profiled)
		w(uint8(0)) // prof_overflow
	}
	if ver >= 5 {
		w(uint16(len(docroot)))
	}
	b.WriteString(host)
	b.WriteString(uri)
	b.WriteString(method)
	b.WriteString(id)
	b.WriteString(phpVer)
	if ver >= 5 {
		b.WriteString(docroot)
	}
	for _, c := range comps {
		w(uint8(len(c.Name)))
		if ver >= 4 {
			w(c.InclNs)
			w(c.SelfNs)
		} else {
			w(uint64(c.Ms * 1e6))
		}
		w(uint32(c.Calls))
		b.WriteString(c.Name)
	}
	return b.Bytes()
}

func TestDeserializeTraceV4Components(t *testing.T) {
	comps := []Component{
		{Name: "plugins/woocommerce", InclNs: 120_000_000, SelfNs: 80_000_000, Calls: 34605},
		{Name: "themes/storefront", InclNs: 30_000_000, SelfNs: 29_000_000, Calls: 1200},
		{Name: "vendor/a/b", InclNs: 5_000_000, SelfNs: 5_000_000, Calls: 7},
	}
	data := buildTraceRecord(4, comps, 1, 1)
	tr, err := DeserializeTrace(data, 4)
	if err != nil {
		t.Fatalf("deserialize v4: %v", err)
	}
	if tr.Host != "shop.local" || tr.URI != "/sklep/" || tr.ID != "pr-1-2-3" || tr.PhpVer != "8.3.33" {
		t.Fatalf("strings mismatch: %+v", tr)
	}
	if tr.App != "wordpress" || tr.Profiled != 1 {
		t.Fatalf("v4 header: app=%q profiled=%d", tr.App, tr.Profiled)
	}
	if len(tr.Components) != 3 {
		t.Fatalf("components: got %d", len(tr.Components))
	}
	for i, c := range tr.Components {
		if c.Name != comps[i].Name || c.InclNs != comps[i].InclNs || c.SelfNs != comps[i].SelfNs || c.Calls != comps[i].Calls {
			t.Errorf("component %d mismatch: got %+v want %+v", i, c, comps[i])
		}
		if c.Ms != float64(c.InclNs)/1e6 {
			t.Errorf("component %d: Ms=%v want incl %v", i, c.Ms, float64(c.InclNs)/1e6)
		}
	}
	// pct = incl / duration: 120 ms of 400 ms = 30 %
	if p := tr.Components[0].Pct; p < 29.99 || p > 30.01 {
		t.Errorf("pct: got %v want 30", p)
	}
}

func TestDeserializeTraceV5Docroot(t *testing.T) {
	comps := []Component{{Name: "plugins/x", InclNs: 1_000_000, SelfNs: 900_000, Calls: 3}}
	data := buildTraceRecord(5, comps, 1, 1)
	tr, err := DeserializeTrace(data, 5)
	if err != nil {
		t.Fatalf("deserialize v5: %v", err)
	}
	if tr.Docroot != "/var/www/html" {
		t.Fatalf("v5 docroot: got %q", tr.Docroot)
	}
	if tr.App != "wordpress" || tr.Profiled != 1 || tr.PhpVer != "8.3.33" || len(tr.Components) != 1 || tr.Components[0].Calls != 3 {
		t.Fatalf("v5 fields after docroot: %+v", tr)
	}
	// v4 record has no docroot
	tr4, err := DeserializeTrace(buildTraceRecord(4, comps, 1, 1), 4)
	if err != nil || tr4.Docroot != "" || tr4.Components[0].Name != "plugins/x" {
		t.Fatalf("v4 compat: %v %+v", err, tr4)
	}
}

func TestDeserializeTraceV4NotProfiled(t *testing.T) {
	data := buildTraceRecord(4, nil, 0, 0)
	tr, err := DeserializeTrace(data, 4)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if tr.Profiled != 0 || tr.App != "" || len(tr.Components) != 0 {
		t.Fatalf("unexpected: profiled=%d app=%q comps=%d", tr.Profiled, tr.App, len(tr.Components))
	}
}

func TestDeserializeTraceV3Compat(t *testing.T) {
	comps := []Component{{Name: "plugins/x", Ms: 12.5, Calls: 99}}
	data := buildTraceRecord(3, comps, 0, 0)
	tr, err := DeserializeTrace(data, 3)
	if err != nil {
		t.Fatalf("deserialize v3: %v", err)
	}
	if len(tr.Components) != 1 || tr.Components[0].Name != "plugins/x" || tr.Components[0].Calls != 99 {
		t.Fatalf("v3 components: %+v", tr.Components)
	}
	if tr.Components[0].Ms != 12.5 || tr.Components[0].InclNs != 0 || tr.Components[0].SelfNs != 0 {
		t.Fatalf("v3 component values: %+v", tr.Components[0])
	}
	// legacy traces with components were produced by the always-on profiler
	if tr.Profiled != 1 {
		t.Fatalf("v3 profiled backfill: got %d", tr.Profiled)
	}
}
