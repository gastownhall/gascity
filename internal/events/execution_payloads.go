package events

func init() {
	RegisterPayload(ExecutionWorkAssociated, NoPayload{})
	RegisterPayload(ExecutionStepDefined, NoPayload{})
	RegisterPayload(ExecutionStepStarted, NoPayload{})
	RegisterPayload(ExecutionStepCompleted, NoPayload{})
}
