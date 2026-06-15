package docker

import (
	"context"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// EventKind classifies an [Event] without leaking the Docker SDK's
// event-type strings to callers.
type EventKind string

const (
	EventKindContainer EventKind = "container"
	EventKindNetwork   EventKind = "network"
	EventKindOther     EventKind = "other"
)

// Event is a daemon-agnostic projection of a Docker event. Only the
// fields multienv actually uses are exposed.
type Event struct {
	Kind    EventKind
	Action  string
	ActorID string
	// Attributes carries the actor's attribute map. For containers this
	// typically includes the container name and labels.
	Attributes map[string]string
}

// Events subscribes to the Docker event stream filtered to container
// and network events. The returned channels follow the SDK's contract:
// the caller cancels ctx to close the stream, and an error on the error
// channel terminates the stream (the caller may resubscribe).
func (c *Client) Events(ctx context.Context) (<-chan Event, <-chan error) {
	filters := client.Filters{}.
		Add("type", string(events.ContainerEventType)).
		Add("type", string(events.NetworkEventType))

	res := c.api.Events(ctx, client.EventsListOptions{Filters: filters})

	out := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-res.Err:
				if err != nil {
					errs <- err
				}
				return
			case msg, ok := <-res.Messages:
				if !ok {
					return
				}
				ev := Event{
					Kind:       kindFromType(msg.Type),
					Action:     string(msg.Action),
					ActorID:    msg.Actor.ID,
					Attributes: msg.Actor.Attributes,
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, errs
}

func kindFromType(t events.Type) EventKind {
	switch t {
	case events.ContainerEventType:
		return EventKindContainer
	case events.NetworkEventType:
		return EventKindNetwork
	default:
		return EventKindOther
	}
}
