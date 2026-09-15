package http

import (
	"sync"
	"sync/atomic"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// SlackEventQueueCapacity is the maximum number of events waiting to be handed
// to each registered processor. When full, new events for that processor are
// dropped so a slow feature cannot delay Slack's webhook acknowledgement.
const SlackEventQueueCapacity = 100

type slackEventDispatcher struct {
	log       *zap.Logger
	processor slackEventProcessor
	queue     chan slackevents.EventsAPIEvent
	stop      chan struct{}
	pending   atomic.Int64
	stateMu   sync.Mutex
	stopped   bool
	inFlight  int
}

func newSlackEventDispatcher(log *zap.Logger, processor slackEventProcessor) *slackEventDispatcher {
	return &slackEventDispatcher{
		log:       log,
		processor: processor,
		queue:     make(chan slackevents.EventsAPIEvent, SlackEventQueueCapacity),
		stop:      make(chan struct{}),
	}
}

func (d *slackEventDispatcher) run() {
	for {
		// Prefer shutdown over taking more buffered work once the drain deadline expires.
		select {
		case <-d.stop:
			return
		default:
		}

		select {
		case <-d.stop:
			return
		case event, ok := <-d.queue:
			if !ok {
				return
			}
			if !d.claim() {
				d.pending.Add(-1)
				return
			}
			d.pushSafely(event)
			d.complete()
		}
	}
}

func (d *slackEventDispatcher) pushSafely(event slackevents.EventsAPIEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.log.Error("Slack event processor panicked",
				zap.String("processor", d.processor.ProcessorType()),
				zap.Any("panic", recovered))
		}
	}()
	d.processor.PushEvent(event)
}

func (d *slackEventDispatcher) enqueue(event slackevents.EventsAPIEvent) bool {
	d.pending.Add(1)
	select {
	case d.queue <- event:
		return true
	default:
		d.pending.Add(-1)
		return false
	}
}

func (d *slackEventDispatcher) claim() bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.stopped {
		return false
	}
	d.inFlight++
	return true
}

func (d *slackEventDispatcher) complete() {
	d.stateMu.Lock()
	d.inFlight--
	d.pending.Add(-1)
	d.stateMu.Unlock()
}

func (d *slackEventDispatcher) abandon() (abandoned, inFlight int) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if !d.stopped {
		d.stopped = true
		close(d.stop)
	}
	inFlight = d.inFlight
	abandoned = int(d.pending.Load()) - inFlight
	return abandoned, inFlight
}
