package extmsg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

const (
	labelBindingBase          = "gc:extmsg-binding"
	labelDeliveryBase         = "gc:extmsg-delivery"
	labelGroupBase            = "gc:extmsg-group"
	labelGroupParticipantBase = "gc:extmsg-group-participant"
	labelTranscriptBase       = "gc:extmsg-transcript"
	labelMembershipBase       = "gc:extmsg-membership"
	labelTranscriptStateBase  = "gc:extmsg-transcript-state"

	labelBindingConversationPrefix = "extmsg:binding:conv:v1:"
	labelBindingSessionPrefix      = "extmsg:binding:session:v1:"
	labelDeliveryRoutePrefix       = "extmsg:delivery:route:v1:"
	labelDeliverySessionPrefix     = "extmsg:delivery:session:v1:"
	labelGroupRootPrefix           = "extmsg:group:root:v1:"
	labelGroupParticipantPrefix    = "extmsg:group:participant:v1:"
	labelGroupParticipantSession   = "extmsg:group:participant:session:v1:"
	labelTranscriptConversation    = "extmsg:transcript:conv:v1:"
	labelTranscriptBucketPrefix    = "extmsg:transcript:bucket:v1:"
	labelTranscriptMessagePrefix   = "extmsg:transcript:msg:v1:"
	labelMembershipConversation    = "extmsg:membership:conv:v1:"
	labelMembershipSessionPrefix   = "extmsg:membership:session:v1:"
	labelMembershipExactPrefix     = "extmsg:membership:exact:v1:"
	labelTranscriptStatePrefix     = "extmsg:transcript:state:v1:"
)

func bindingConversationLabel(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return labelBindingConversationPrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
	)
}

func bindingSessionLabel(sessionID string) string {
	return labelBindingSessionPrefix + strings.TrimSpace(sessionID)
}

func deliveryRouteLabel(ref ConversationRef, sessionID string) string {
	ref = normalizeConversationRef(ref)
	return labelDeliveryRoutePrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
		strings.TrimSpace(sessionID),
	)
}

func deliverySessionLabel(sessionID string) string {
	return labelDeliverySessionPrefix + strings.TrimSpace(sessionID)
}

func groupRootLabel(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return labelGroupRootPrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
	)
}

func groupParticipantLabel(groupID string) string {
	return labelGroupParticipantPrefix + strings.TrimSpace(groupID)
}

func groupParticipantSessionLabel(sessionID string) string {
	return labelGroupParticipantSession + strings.TrimSpace(sessionID)
}

func transcriptConversationLabel(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return labelTranscriptConversation + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
	)
}

func transcriptBucketLabel(ref ConversationRef, bucket int64) string {
	ref = normalizeConversationRef(ref)
	return labelTranscriptBucketPrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
		strconv.FormatInt(bucket, 10),
	)
}

func transcriptProviderMessageLabel(ref ConversationRef, providerMessageID string) string {
	ref = normalizeConversationRef(ref)
	return labelTranscriptMessagePrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
		strings.TrimSpace(providerMessageID),
	)
}

func membershipConversationLabel(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return labelMembershipConversation + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
	)
}

func membershipSessionLabel(sessionID string) string {
	return labelMembershipSessionPrefix + strings.TrimSpace(sessionID)
}

func membershipExactLabel(ref ConversationRef, sessionID string) string {
	ref = normalizeConversationRef(ref)
	return labelMembershipExactPrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
		strings.TrimSpace(sessionID),
	)
}

func transcriptStateLabel(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return labelTranscriptStatePrefix + hashJoin(
		ref.ScopeID,
		ref.Provider,
		ref.AccountID,
		ref.ConversationID,
		ref.ParentConversationID,
		string(ref.Kind),
	)
}

func conversationLockKey(ref ConversationRef) string {
	return bindingConversationLabel(ref)
}

func hashJoin(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
