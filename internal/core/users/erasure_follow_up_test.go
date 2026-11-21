package users

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// recordingErasureFollowUp records the DIDs AfterErasure was called with.
type recordingErasureFollowUp struct {
	mu   sync.Mutex
	dids []string
}

func (f *recordingErasureFollowUp) AfterErasure(_ context.Context, did string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dids = append(f.dids, did)
}

func (f *recordingErasureFollowUp) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dids...)
}

// B9: DeleteAccount runs the erasure follow-up after a committed erasure, and
// not after a failed one.
func TestDeleteAccount_RunsTheErasureFollowUpOnlyAfterASuccessfulErasure(t *testing.T) {
	tests := []struct {
		name      string
		deleteErr error
		want      []string
	}{
		{name: "successful erasure", want: []string{"did:plc:erasedmoderator"}},
		{name: "failed erasure", deleteErr: errors.New("database error"), want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockRepo := new(MockUserRepository)
			mockRepo.On("GetByDID", mock.Anything, "did:plc:erasedmoderator").Return(&User{
				DID: "did:plc:erasedmoderator", Handle: "erasedmoderator.test", PDSURL: "https://test.pds", CreatedAt: time.Now(),
			}, nil)
			mockRepo.On("Delete", mock.Anything, "did:plc:erasedmoderator").Return(tc.deleteErr)
			followUp := &recordingErasureFollowUp{}

			service := NewUserService(mockRepo, new(MockIdentityResolver), "https://default.pds", nil, "", WithErasureFollowUp(followUp))
			err := service.DeleteAccount(context.Background(), "did:plc:erasedmoderator")

			if tc.deleteErr != nil {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, followUp.called(), "erasure follow-up calls")
			mockRepo.AssertExpectations(t)
		})
	}
}
