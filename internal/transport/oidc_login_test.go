package transport

import "testing"

func TestOIDCFailureKeepsOnlyLocalReturnDestination(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"/authorize?request_id=req_aaaaaaaaaaaaaaaaaaaaaa", "/login?oidc_failed=true&return_request=req_aaaaaaaaaaaaaaaaaaaaaa"},
		{"/w/test/apps", "/login?oidc_failed=true&redirect=%2Fw%2Ftest%2Fapps"},
		{"//outside.example", "/login?oidc_failed=true"},
	} {
		if got := oidcRetryPath(tc.target); got != tc.want {
			t.Errorf("登录回退地址=%q，需要 %q", got, tc.want)
		}
	}
}
