package events

func init() {
	RegisterPayload(ExecutionWorkAssociated, NoPayload{})
	RegisterPayload(ExecutionStepDefined, NoPayload{})
}
