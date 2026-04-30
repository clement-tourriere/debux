package runtime

import "testing"

func TestParseK8sTargetNamespaceSemantics(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantNS    string
		wantName  string
		wantCtr   string
		wantError bool
	}{
		{name: "picker current namespace", raw: "k8s://"},
		{name: "pod current namespace", raw: "k8s://api", wantName: "api"},
		{name: "explicit default namespace", raw: "k8s://default/api", wantNS: "default", wantName: "api"},
		{name: "explicit namespace picker", raw: "k8s://prod/", wantNS: "prod"},
		{name: "explicit container", raw: "k8s://prod/api/app", wantNS: "prod", wantName: "api", wantCtr: "app"},
		{name: "missing namespace", raw: "k8s:///api", wantError: true},
		{name: "empty container", raw: "k8s://prod/api/", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseTarget(tt.raw)
			if tt.wantError {
				if err == nil {
					t.Fatalf("ParseTarget(%q) succeeded, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.raw, err)
			}
			if target.Runtime != "kubernetes" {
				t.Fatalf("Runtime = %q, want kubernetes", target.Runtime)
			}
			if target.Namespace != tt.wantNS || target.Name != tt.wantName || target.Container != tt.wantCtr {
				t.Fatalf("target = %#v, want namespace=%q name=%q container=%q", target, tt.wantNS, tt.wantName, tt.wantCtr)
			}
		})
	}
}
