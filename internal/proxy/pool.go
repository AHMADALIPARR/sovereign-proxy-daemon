package proxy

import (
	"sync"
	"sync/atomic"
)

type ProxyNode struct {
	URL       string
	IsHealthy atomic.Bool
	LatencyMs atomic.Int64
}

type ProxyPool struct {
	mu     sync.RWMutex
	nodes  []*ProxyNode
	cursor uint64
}

func NewProxyPool(urls []string) *ProxyPool {
	var nodes []*ProxyNode
	for _, u := range urls {
		node := &ProxyNode{URL: u}
		node.IsHealthy.Store(true) // assume healthy initially
		nodes = append(nodes, node)
	}
	return &ProxyPool{nodes: nodes}
}

func (p *ProxyPool) GetHealthyNodes() []*ProxyNode {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthy []*ProxyNode
	for _, node := range p.nodes {
		if node.IsHealthy.Load() {
			healthy = append(healthy, node)
		}
	}
	return healthy
}

func (p *ProxyPool) Next() *ProxyNode {
	healthy := p.GetHealthyNodes()
	if len(healthy) == 0 {
		return nil
	}
	idx := atomic.AddUint64(&p.cursor, 1)
	return healthy[idx%uint64(len(healthy))]
}

func (p *ProxyPool) GetAll() []*ProxyNode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.nodes
}
