package selector

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
)

const (
	WRR = "wrr"
)

// WeightedItem defines the interface that items managed by the generic WRR selector must implement.
type WeightedItem interface {
	Item
	// GetConfigWeight returns the configured weight of the item.
	GetConfigWeight() int
	// GetCurrentWeight returns the current weight of the item (used by the WRR algorithm).
	GetCurrentWeight() int
	// SetCurrentWeight sets the current weight of the item.
	SetCurrentWeight(int)
}

// WeightedRoundRobinSelector is a generic implementation of the Smooth Weighted Round Robin algorithm.
type WeightedRoundRobinSelector[T WeightedItem] struct {
	items             []T
	totalConfigWeight int
	mu                *sync.Mutex
	logger            *logrus.Entry
}

// NewWeightedRoundRobinSelector creates a new generic WeightedRoundRobinSelector.
func NewWeightedRoundRobinSelector[T WeightedItem]() *WeightedRoundRobinSelector[T] {
	return &WeightedRoundRobinSelector[T]{
		items:  make([]T, 0),
		mu:     &sync.Mutex{},
		logger: logrus.WithField("selector", WRR),
	}
}

// AddItem adds an item to the selector.
func (s *WeightedRoundRobinSelector[T]) AddItem(item T) {
	s.mu.Lock()
	s.items = append(s.items, item)
	s.totalConfigWeight += item.GetConfigWeight()
	s.logger.Infof("added WRR item '%s', weight: %d", item.GetName(), item.GetConfigWeight())
	s.mu.Unlock()
}

// Select chooses an item based on the Smooth Weighted Round Robin algorithm.
// It returns the selected item or an error if no item is available or all are disabled.
func (s *WeightedRoundRobinSelector[T]) Select() (item T, err error) {
	s.logger.Trace("attempting to acquire wrr lock")
	s.mu.Lock()
	s.logger.Trace("acquired wrr lock")

	defer func() {
		s.mu.Unlock()
		s.logger.Trace("released wrr lock")
	}()

	if len(s.items) == 0 {
		return item, fmt.Errorf("no items available in selector")
	}

	selectedIndex := -1
	maxCurrentWeight := 0
	wrrBefore := s.unsafeString()

	// Nginx's smooth weighted round-robin (sWRR) algorithm:
	for i := range s.items {
		// Use index to get a mutable copy if T is a struct
		entry := s.items[i]
		if entry.IsDisabled() {
			// Skip disabled item
			continue
		}

		// sWRR: 1. For each server i: current_weight[i] = current_weight[i] + effective_weight[i]
		entry.SetCurrentWeight(entry.GetCurrentWeight() + entry.GetConfigWeight())

		if selectedIndex == -1 || entry.GetCurrentWeight() > maxCurrentWeight {
			// sWRR: 2. selected_server = server with highest current_weight
			maxCurrentWeight = entry.GetCurrentWeight()
			selectedIndex = i
		}
	}

	if selectedIndex == -1 {
		return item, fmt.Errorf("no available item")
	}

	selectedItem := s.items[selectedIndex]
	// sWRR: 3. current_weight[selected_server] = current_weight[selected_server] - total_weight
	selectedItem.SetCurrentWeight(selectedItem.GetCurrentWeight() - s.totalConfigWeight)

	wrrAfter := s.unsafeString()
	s.logger.Tracef("wrr before: %s", wrrBefore)
	s.logger.Tracef("wrr after: %s", wrrAfter)

	// Update the item in the slice if T is a struct
	s.items[selectedIndex] = selectedItem

	s.logger.Debugf("selected item: %s", selectedItem.GetName())
	return selectedItem, nil
}

// TotalConfigWeight returns the sum of configured weights of all items.
func (s *WeightedRoundRobinSelector[T]) TotalConfigWeight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalConfigWeight
}

func (s *WeightedRoundRobinSelector[T]) unsafeString() string {
	m := map[string]int{}
	for _, item := range s.items {
		m[item.GetName()] = item.GetCurrentWeight()
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (s *WeightedRoundRobinSelector[T]) GetType() string {
	return WRR
}
