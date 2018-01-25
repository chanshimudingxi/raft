package raft

import (
	"reflect"
	"sync"
)

//事件派发器职责是管理命名事件的监听者，同时发送事件通知给监听者
// eventDispatcher is responsible for managing listeners for named events
// and dispatching event notifications to those listeners.
type eventDispatcher struct {
	sync.RWMutex //监听者监听的事件是一个队列，用于数据保护。用读写锁，难道读的次数远高于写的次数？
	source       interface{}
	listeners    map[string]eventListeners
}

// EventListener is a function that can receive event notifications.
type EventListener func(Event)

// EventListeners represents a collection of individual listeners.
type eventListeners []EventListener

// newEventDispatcher creates a new eventDispatcher instance.
func newEventDispatcher(source interface{}) *eventDispatcher {
	return &eventDispatcher{
		source:    source,
		listeners: make(map[string]eventListeners),
	}
}

// AddEventListener adds a listener function for a given event type.
func (d *eventDispatcher) AddEventListener(typ string, listener EventListener) {
	d.Lock()
	defer d.Unlock()
	d.listeners[typ] = append(d.listeners[typ], listener)
}

// RemoveEventListener removes a listener function for a given event type.
func (d *eventDispatcher) RemoveEventListener(typ string, listener EventListener) {
	d.Lock()
	defer d.Unlock()

	// Grab a reference to the function pointer once.
	ptr := reflect.ValueOf(listener).Pointer()

	// Find listener by pointer and remove it.
	listeners := d.listeners[typ]
	for i, l := range listeners {
		if reflect.ValueOf(l).Pointer() == ptr {
			d.listeners[typ] = append(listeners[:i], listeners[i+1:]...)
		}
	}
}

// DispatchEvent dispatches an event.
func (d *eventDispatcher) DispatchEvent(e Event) {
	d.RLock()
	defer d.RUnlock()

	// Automatically set the event source.
	if e, ok := e.(*event); ok {
		e.source = d.source
	}

	// Dispatch the event to all listeners.
	for _, l := range d.listeners[e.Type()] {
		l(e)
	}
}
