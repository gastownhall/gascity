package api

// BeadAttemptsDiffInput is the Huma input for
// GET /v0/city/{cityName}/bead/{id}/attempts/diff.
type BeadAttemptsDiffInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}
