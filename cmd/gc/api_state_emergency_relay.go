package main

import (
	"context"
	"log"

	"github.com/gastownhall/gascity/internal/emergency"
)

// startEmergencyEventRelay launches a goroutine that drains emergencyCh and
// mirrors each record into the city event log as an emergency.signaled event.
// Returns immediately if emergencyCh or eventProv is nil.
func (cs *controllerState) startEmergencyEventRelay(ctx context.Context) {
	if cs.emergencyCh == nil || cs.eventProv == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rec, ok := <-cs.emergencyCh:
				if !ok {
					return
				}
				if err := emergency.RecordSignaled(cs.eventProv, rec); err != nil {
					log.Printf("api: emergency relay: %v", err)
				}
			}
		}
	}()
}
