package runtime

import "testing"

func TestParseK8sTargetNamespaceSemantics(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCtx   string
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
		{name: "context picker", raw: "k8s://@eks-preprod-01", wantCtx: "eks-preprod-01"},
		{name: "context pod current namespace", raw: "k8s://@eks-preprod-01/api", wantCtx: "eks-preprod-01", wantName: "api"},
		{name: "context namespace pod", raw: "k8s://@eks-preprod-01/prod/api", wantCtx: "eks-preprod-01", wantNS: "prod", wantName: "api"},
		{name: "context namespace pod container", raw: "k8s://@eks-preprod-01/prod/api/app", wantCtx: "eks-preprod-01", wantNS: "prod", wantName: "api", wantCtr: "app"},
		{name: "escaped context", raw: "k8s://@arn%3Aaws%3Aeks%3Aus-west-2%3A123%3Acluster%2Fprod/prod/api", wantCtx: "arn:aws:eks:us-west-2:123:cluster/prod", wantNS: "prod", wantName: "api"},
		{name: "empty context", raw: "k8s://@/prod/api", wantError: true},
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
			if target.Context != tt.wantCtx || target.Namespace != tt.wantNS || target.Name != tt.wantName || target.Container != tt.wantCtr {
				t.Fatalf("target = %#v, want context=%q namespace=%q name=%q container=%q", target, tt.wantCtx, tt.wantNS, tt.wantName, tt.wantCtr)
			}
		})
	}
}
