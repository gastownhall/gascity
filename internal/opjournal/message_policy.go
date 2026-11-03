package opjournal

// NormalizeMessagePolicy applies the existing message-kind defaults and
// validation without admitting an operation. Public-room admission uses it to
// freeze a recipient's real Sessions policy before committing its outbox item.
// It deliberately has no independent vocabulary or steer fallback behavior.
func NormalizeMessagePolicy(message MessagePolicy) (MessagePolicy, error) {
	spec, err := Lookup(KindMessage)
	if err != nil {
		return MessagePolicy{}, err
	}
	policy := applyPolicyDefaults(Policy{Message: message}, spec)
	if err := validatePolicy(policy, spec); err != nil {
		return MessagePolicy{}, err
	}
	return policy.Message, nil
}
