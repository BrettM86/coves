package postgres

import "testing"

// TestAccountSubjectDID pins which subject URIs name an account: only
// at://<did>, which takes the ban-subject lock in LockSubject.
func TestAccountSubjectDID(t *testing.T) {
	for _, test := range []struct {
		name, subjectURI, wantDID string
		wantAccount               bool
	}{
		{"account subject", "at://did:plc:x", "did:plc:x", true},
		{"record subject", "at://did:plc:x/social.coves.community.comment/abc", "", false},
		{"handle authority", "at://alice.example.com", "", false},
		{"bare DID", "did:plc:x", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			did, account := accountSubjectDID(test.subjectURI)
			if did != test.wantDID || account != test.wantAccount {
				t.Errorf("accountSubjectDID(%q) = (%q, %v), want (%q, %v)", test.subjectURI, did, account, test.wantDID, test.wantAccount)
			}
		})
	}
}
