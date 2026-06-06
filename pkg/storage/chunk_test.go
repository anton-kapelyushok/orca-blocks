package storage

import "testing"

func TestChunkIndexesSpanningBoundaries(t *testing.T) {
	got, err := ChunkIndexes(6, 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 ranges, got %d", len(got))
	}
	if got[0].Index != 0 || got[0].ChunkStart != 6 || got[0].ReqStart != 6 || got[0].ReqEnd != 8 {
		t.Fatalf("unexpected first range: %+v", got[0])
	}
	if got[1].Index != 1 || got[1].ChunkStart != 0 || got[1].ReqStart != 8 || got[1].ReqEnd != 16 {
		t.Fatalf("unexpected second range: %+v", got[1])
	}
}

func TestManifestWireRoundTrip(t *testing.T) {
	manifest := Manifest{2: "chunk-b", 10: "chunk-j"}
	got, err := ManifestFromWire(ManifestToWire(manifest))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range manifest {
		if got[k] != v {
			t.Fatalf("manifest[%d] = %q, want %q", k, got[k], v)
		}
	}
}
