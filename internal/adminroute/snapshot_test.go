package adminroute

import "testing"

func TestSnapshotLookup(t *testing.T) {
	snapshot := NewSnapshot()
	if upstream, ok := snapshot.Lookup("play.example"); ok || upstream != "" {
		t.Fatalf("Lookup(empty) = %q, %v; want miss", upstream, ok)
	}

	snapshot.Store(map[string]string{
		"play.example": "127.0.0.1:25565",
		"default":      "127.0.0.1:25566",
	})

	if upstream, ok := snapshot.Lookup("play.example"); !ok || upstream != "127.0.0.1:25565" {
		t.Fatalf("Lookup(play.example) = %q, %v", upstream, ok)
	}
	if upstream, ok := snapshot.Lookup("unknown.example"); !ok || upstream != "127.0.0.1:25566" {
		t.Fatalf("Lookup(default fallback) = %q, %v", upstream, ok)
	}
}

func TestSnapshotStoreCopiesRoutes(t *testing.T) {
	snapshot := NewSnapshot()
	routes := map[string]string{"play.example": "127.0.0.1:25565"}
	snapshot.Store(routes)
	routes["play.example"] = "changed"

	if upstream, ok := snapshot.Lookup("play.example"); !ok || upstream != "127.0.0.1:25565" {
		t.Fatalf("Lookup(after source mutation) = %q, %v", upstream, ok)
	}
}

func TestSnapshotCloneCopiesRoutes(t *testing.T) {
	snapshot := NewSnapshot()
	if clone := snapshot.Clone(); clone != nil {
		t.Fatalf("Clone(empty) = %#v, want nil", clone)
	}

	snapshot.Store(map[string]string{"play.example": "127.0.0.1:25565"})
	clone := snapshot.Clone()
	clone["play.example"] = "changed"

	if upstream, ok := snapshot.Lookup("play.example"); !ok || upstream != "127.0.0.1:25565" {
		t.Fatalf("Lookup(after clone mutation) = %q, %v", upstream, ok)
	}
}
