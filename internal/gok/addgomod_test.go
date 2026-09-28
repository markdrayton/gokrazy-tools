package gok

import (
	"slices"
	"testing"
)

func TestFirstProxyURL(t *testing.T) {
	for _, tt := range []struct {
		goproxy string
		want    string
		wantOK  bool
	}{
		{"https://proxy.golang.org", "https://proxy.golang.org", true},
		{"https://proxy.golang.org,direct", "https://proxy.golang.org", true},
		{"https://proxy.example/|https://proxy.golang.org", "https://proxy.example", true},
		{"direct", "", false},
		{"off", "", false},
		{"", "", false},
	} {
		got, ok := firstProxyURL(tt.goproxy)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("firstProxyURL(%q) = %q, %v; want %q, %v", tt.goproxy, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestCandidateModulePaths(t *testing.T) {
	got := candidateModulePaths("github.com/user/project/cmd/project")
	want := []string{
		"github.com/user/project/cmd/project",
		"github.com/user/project/cmd",
		"github.com/user/project",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidateModulePaths = %q, want %q", got, want)
	}
}
