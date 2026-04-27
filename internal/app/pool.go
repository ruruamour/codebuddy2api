package app

import (
	"errors"
	"sort"
	"sync"
)

var ErrNoAccountAvailable = errors.New("no enabled CodeBuddy accounts available")
var ErrAllAccountsBusy = errors.New("all CodeBuddy accounts are at concurrency limit")

type Lease struct {
	Account Account
}

type Pool struct {
	store    *Store
	mu       sync.Mutex
	inFlight map[int64]int
	cursor   int
}

func NewPool(store *Store) *Pool {
	return &Pool{
		store:    store,
		inFlight: make(map[int64]int),
	}
}

func (p *Pool) Acquire() (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	candidates, err := p.store.SchedulableAccounts()
	if err != nil {
		return Lease{}, err
	}
	if len(candidates) == 0 {
		return Lease{}, ErrNoAccountAvailable
	}

	priorities := make(map[int]struct{})
	for _, account := range candidates {
		priorities[account.Priority] = struct{}{}
	}
	sortedPriorities := make([]int, 0, len(priorities))
	for priority := range priorities {
		sortedPriorities = append(sortedPriorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sortedPriorities)))

	for _, priority := range sortedPriorities {
		var samePriority []Account
		for _, account := range candidates {
			if account.Priority == priority {
				samePriority = append(samePriority, account)
			}
		}
		weighted := weightedCandidates(samePriority)
		total := len(weighted)
		for offset := 0; offset < total; offset++ {
			index := (p.cursor + offset) % total
			account := weighted[index]
			if p.inFlight[account.ID] >= account.Concurrency {
				continue
			}
			p.inFlight[account.ID]++
			p.cursor = (index + 1) % total
			return Lease{Account: account}, nil
		}
	}

	return Lease{}, ErrAllAccountsBusy
}

func (p *Pool) Release(lease Lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.inFlight[lease.Account.ID]
	if current <= 1 {
		delete(p.inFlight, lease.Account.ID)
		return
	}
	p.inFlight[lease.Account.ID] = current - 1
}

func (p *Pool) Snapshot() map[int64]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make(map[int64]int, len(p.inFlight))
	for key, value := range p.inFlight {
		result[key] = value
	}
	return result
}

func weightedCandidates(accounts []Account) []Account {
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].ID < accounts[j].ID
	})
	var result []Account
	for _, account := range accounts {
		weight := max(1, account.Weight)
		for i := 0; i < weight; i++ {
			result = append(result, account)
		}
	}
	return result
}
