package authorization

import "testing"

func TestFixedRoleMatrix(t *testing.T) {
	p, err := NewPolicy()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		role   string
		action Action
		want   bool
	}{
		{"viewer", BuildRead, true}, {"viewer", LogsRead, true}, {"viewer", SourceRead, false}, {"viewer", BuildWrite, false},
		{"developer", SourceUpload, true}, {"developer", DeploymentWrite, true}, {"developer", RuntimeWrite, false}, {"developer", ConfigWrite, false}, {"developer", SecretWrite, false},
		{"maintainer", RuntimeWrite, true}, {"maintainer", ConfigWrite, true}, {"maintainer", SecretWrite, true}, {"maintainer", MemberWrite, false}, {"maintainer", ApplicationWrite, false},
		{"workspace_admin", MemberWrite, true}, {"workspace_admin", ApplicationWrite, true}, {"instance_admin", SourceRead, true}, {"instance_admin", DeploymentWrite, true},
		{"toString", BuildRead, false}, {"*", ConfigWrite, false},
	}
	for _, tc := range cases {
		t.Run(tc.role+"/"+string(tc.action), func(t *testing.T) {
			if got := p.Allows(tc.role, tc.action); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestTokenHasNoImplicitWildcard(t *testing.T) {
	p := Principal{UserID: "user", TokenID: "pat", InstanceAdmin: true, Scopes: []string{"build:read"}}
	if p.AllowsScope("app", BuildRead) {
		t.Fatal("空应用范围不能解释为全部")
	}
	p.ApplicationIDs = []string{"app"}
	if !p.AllowsScope("app", BuildRead) || p.AllowsScope("other", BuildRead) || p.AllowsScope("app", BuildWrite) {
		t.Fatal("凭证范围未取交集")
	}
}
