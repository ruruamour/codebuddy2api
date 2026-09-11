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
	store                *Store
	types                *TypeRegistry
	fallbackModels       []string
	fallbackPoolStrategy string
	mu                   sync.Mutex
	inFlight             map[int64]int
	cursor               int
}

func NewPool(store *Store, types *TypeRegistry, fallbackModels []string, fallbackPoolStrategy string) *Pool {
	if types == nil {
		types = NewTypeRegistry()
	}
	return &Pool{
		store:                store,
		types:                types,
		fallbackModels:       append([]string{}, fallbackModels...),
		fallbackPoolStrategy: NormalizePoolStrategy(fallbackPoolStrategy, PoolStrategyRoundRobin),
		inFlight:             make(map[int64]int),
	}
}

// ErrNoAccountForModel 有可用账号，但没有一个声明支持这个模型。
// 和「一个号都没有」分开报，否则合并面板后排查起来全是瞎猜。
var ErrNoAccountForModel = errors.New("no account serves the requested model")

// Acquire 取一个能承接该模型的账号。
// model 传空表示不限（老调用方行为不变）。
func (p *Pool) Acquire(model string) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	all, err := p.store.SchedulableAccounts()
	if err != nil {
		return Lease{}, err
	}
	if len(all) == 0 {
		return Lease{}, ErrNoAccountAvailable
	}

	// 模型路由：能接哪些模型由账号类型决定，账号自己声明了才以账号为准。
	// 这是「一个实例同时挂 CN 和 intl 账号」能成立的前提——
	// glm-5.1 的请求不能被路由到只有免费 deepseek 的 intl 凭证号上。
	//
	// 类型停用等同于把它名下的号整体下线：停用一个渠道是运维动作，
	// 要能一键生效，而不是去逐个关号。
	candidates := make([]Account, 0, len(all))
	for _, account := range all {
		typ := p.types.TypeOf(account)
		if typ != nil && !typ.Enabled {
			continue
		}
		if !servesModel(account, typ, model) {
			continue
		}
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return Lease{}, ErrNoAccountForModel
	}

	strategy := p.poolStrategy()
	if strategy == PoolStrategyFillFirst {
		return p.acquireFillFirst(candidates)
	}
	return p.acquireRoundRobin(candidates)
}

// priorityOf 是账号在池子里的实际优先级（叠加了类型优先级）。
func (p *Pool) priorityOf(account Account) int {
	return effectivePriority(account, p.types.TypeOf(account))
}

func (p *Pool) poolStrategy() string {
	settings, err := p.store.ModelSettings(p.fallbackModels, p.fallbackPoolStrategy)
	if err != nil {
		return p.fallbackPoolStrategy
	}
	return NormalizePoolStrategy(settings.PoolStrategy, p.fallbackPoolStrategy)
}

func (p *Pool) acquireRoundRobin(candidates []Account) (Lease, error) {
	priorities := make(map[int]struct{})
	for _, account := range candidates {
		priorities[p.priorityOf(account)] = struct{}{}
	}
	sortedPriorities := make([]int, 0, len(priorities))
	for priority := range priorities {
		sortedPriorities = append(sortedPriorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sortedPriorities)))

	for _, priority := range sortedPriorities {
		var samePriority []Account
		for _, account := range candidates {
			if p.priorityOf(account) == priority {
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

func (p *Pool) acquireFillFirst(candidates []Account) (Lease, error) {
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if lp, rp := p.priorityOf(left), p.priorityOf(right); lp != rp {
			return lp > rp
		}
		if left.Weight != right.Weight {
			return left.Weight > right.Weight
		}
		return left.ID < right.ID
	})
	for _, account := range candidates {
		if p.inFlight[account.ID] >= account.Concurrency {
			continue
		}
		p.inFlight[account.ID]++
		return Lease{Account: account}, nil
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
