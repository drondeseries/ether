package vault

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/store"
)

const fixture = `<nzb><file subject="Movie.2020.mkv" poster="A"><groups><group>test</group></groups><segments><segment bytes="512" number="1">aa@a</segment><segment bytes="510" number="2">bb@b</segment><segment bytes="510" number="2">fill@c</segment></segments></file></nzb>`

func TestPackRecovery(t *testing.T) {
	ctx := context.Background()
	o := Options{Dir: t.TempDir(), DB: store.OpenTest(t)}
	s, e := Open(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	p := PutRequest{NZB: []byte(fixture), Release: core.Release{ID: "first", IndexerID: "a"}}
	k, e := s.Put(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	before, e := s.Stats(ctx)
	if e != nil {
		t.Fatal(e)
	}
	p.Release.IndexerID = "b"
	again, e := s.Put(ctx, p)
	if e != nil || again != k {
		t.Fatal("dedupe", e)
	}
	after, e := s.Stats(ctx)
	if e != nil || before.Bytes != after.Bytes {
		t.Fatal("duplicate payload", e)
	}
	l, e := s.(*library).loc(ctx, o.DB, k)
	if e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(filepath.Join(o.Dir, "packs", l.pack), os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write([]byte("EV01\x03"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	alias, ok, e := s.ByAlias(ctx, "b", "first")
	if !ok || alias != k || e != nil {
		t.Fatal("alias", e)
	}
	r, e := s.Get(ctx, k)
	if e != nil {
		t.Fatal(e)
	}
	want, _ := nzb.Parse(p.NZB)
	if !reflect.DeepEqual(want, r.NZB) {
		t.Fatalf("round trip changed document: %#v vs %#v", want, r.NZB)
	}
	if _, e = nzb.Parse(r.XML); e != nil {
		t.Fatal(e)
	}
}
func FuzzDecode(f *testing.F) {
	d, _ := nzb.Parse([]byte(fixture))
	b, e := Encode(&Record{Kind: KindUsenet, NZB: d})
	if e != nil {
		f.Fatal(e)
	}
	f.Add(b)
	dec, e := decoder()
	if e != nil {
		f.Fatal(e)
	}
	raw, e := dec.DecodeAll(b, nil)
	if e != nil {
		f.Fatal(e)
	}
	f.Add(raw)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Decode(b); _, _ = decodeRaw(b) })
}
