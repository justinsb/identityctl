package workloadidentity

import (
	"encoding/base64"
	"strings"
	"testing"
)

func fakeJWT(t *testing.T, payload string) string {
	t.Helper()
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return encode(`{"alg":"RS256"}`) + "." + encode(payload) + ".signature"
}

func TestProviderAudience(t *testing.T) {
	providerAud := "https://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/kubernetes/providers/mycluster"

	grid := []struct {
		name    string
		token   string
		want    string
		wantErr string
	}{
		{
			name:  "string audience",
			token: `{"aud":"` + providerAud + `","sub":"system:serviceaccount:ns:sa"}`,
			want:  providerAud,
		},
		{
			name:  "array audience",
			token: `{"aud":["` + providerAud + `"]}`,
			want:  providerAud,
		},
		{
			name:  "array with other audiences",
			token: `{"aud":["https://kubernetes.default.svc","` + providerAud + `"]}`,
			want:  providerAud,
		},
		{
			name:    "kubernetes api audience only",
			token:   `{"aud":"https://kubernetes.default.svc"}`,
			wantErr: "does not identify a workload identity provider",
		},
		{
			name:    "no audience",
			token:   `{"sub":"system:serviceaccount:ns:sa"}`,
			wantErr: "does not identify a workload identity provider",
		},
	}

	for _, testCase := range grid {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := providerAudience(fakeJWT(t, testCase.token))
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("got error %v, want error containing %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != testCase.want {
				t.Errorf("got audience %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestProviderAudienceNotAJWT(t *testing.T) {
	if _, err := providerAudience("not-a-jwt"); err == nil {
		t.Fatal("expected error for non-JWT token")
	}
}
