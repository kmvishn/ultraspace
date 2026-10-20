package sysmem

import "testing"

func TestReadAndPick(t *testing.T) {
	in := Read()
	if in.Total == 0 || in.Available == 0 || in.Available > in.Total {
		t.Fatalf("meminfo = %+v", in)
	}
	if p := Pick("tiny"); p.LiveTabs != 1 || p.RendererLimit != 1 {
		t.Fatalf("tiny = %+v", p)
	}
	if p := Pick("auto"); p.Name == "" {
		t.Fatalf("auto picked nothing")
	}
}
