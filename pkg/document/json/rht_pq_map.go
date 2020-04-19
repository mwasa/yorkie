/*
 * Copyright 2020 The Yorkie Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package json

import (
	"fmt"
	"github.com/yorkie-team/yorkie/pkg/document/time"
	"github.com/yorkie-team/yorkie/pkg/log"
	"github.com/yorkie-team/yorkie/pkg/pq"
	"sort"
	"strings"
)

type RHTPQMapNode struct {
	key  string
	elem Element
}

func newRHTPQMapNode(key string, elem Element) *RHTPQMapNode {
	return &RHTPQMapNode{
		key:  key,
		elem: elem,
	}
}

func (n *RHTPQMapNode) Remove(removedAt *time.Ticket) {
	n.elem.Remove(removedAt)
}

func (n *RHTPQMapNode) Less(other pq.Value) bool {
	node := other.(*RHTPQMapNode)
	return n.elem.CreatedAt().After(node.elem.CreatedAt())
}

func (n *RHTPQMapNode) isRemoved() bool {
	return n.elem.RemovedAt() != nil
}

func (n *RHTPQMapNode) Key() string {
	return n.key
}

func (n *RHTPQMapNode) Element() Element {
	return n.elem
}

// RHTPriorityQueueMap is replicated hash table.
type RHTPriorityQueueMap struct {
	nodeQueueMapByKey  map[string]*pq.PriorityQueue
	nodeMapByCreatedAt map[string]*RHTPQMapNode
}

// NewRHTPriorityQueueMap creates a new instance of RHTPriorityQueueMap.
func NewRHTPriorityQueueMap() *RHTPriorityQueueMap {
	return &RHTPriorityQueueMap{
		nodeQueueMapByKey:  make(map[string]*pq.PriorityQueue),
		nodeMapByCreatedAt: make(map[string]*RHTPQMapNode),
	}
}

// Get returns the value of the given key.
func (rht *RHTPriorityQueueMap) Get(key string) Element {
	queue, ok := rht.nodeQueueMapByKey[key]
	if !ok {
		return nil
	}

	node := queue.Peek().(*RHTPQMapNode)
	if node.isRemoved() {
		return nil
	}
	return node.elem
}

// Has returns whether the element exists of the given key or not.
func (rht *RHTPriorityQueueMap) Has(key string) bool {
	queue, ok := rht.nodeQueueMapByKey[key]
	if !ok {
		return false
	}

	node := queue.Peek().(*RHTPQMapNode)
	return node != nil && !node.isRemoved()
}

// Set sets the value of the given key.
func (rht *RHTPriorityQueueMap) Set(k string, v Element) {
	if _, ok := rht.nodeQueueMapByKey[k]; !ok {
		rht.nodeQueueMapByKey[k] = pq.NewPriorityQueue()
	}

	node := newRHTPQMapNode(k, v)
	rht.nodeQueueMapByKey[k].Push(node)
	rht.nodeMapByCreatedAt[v.CreatedAt().Key()] = node
}

// Remove deletes the Element of the given key.
func (rht *RHTPriorityQueueMap) Delete(k string, deletedAt *time.Ticket) Element {
	queue, ok := rht.nodeQueueMapByKey[k]
	if !ok {
		return nil
	}

	node := queue.Peek().(*RHTPQMapNode)
	node.Remove(deletedAt)
	return node.elem
}

// DeleteByCreatedAt deletes the Element of the given creation time.
func (rht *RHTPriorityQueueMap) DeleteByCreatedAt(createdAt *time.Ticket, deletedAt *time.Ticket) Element {
	node, ok := rht.nodeMapByCreatedAt[createdAt.Key()]
	if !ok {
		log.Logger.Warn("fail to find " + createdAt.Key())
		return nil
	}

	node.Remove(deletedAt)
	return node.elem
}

// Elements returns a map of elements because the map easy to use for loop.
// TODO If we encounter performance issues, we need to replace this with other solution.
func (rht *RHTPriorityQueueMap) Elements() map[string]Element {
	members := make(map[string]Element)
	for _, queue := range rht.nodeQueueMapByKey {
		if node := queue.Peek().(*RHTPQMapNode); !node.isRemoved() {
			members[node.key] = node.elem
		}
	}

	return members
}

// AllNodes returns a map of elements because the map easy to use for loop.
// TODO If we encounter performance issues, we need to replace this with other solution.
func (rht *RHTPriorityQueueMap) AllNodes() []*RHTPQMapNode {
	var nodes []*RHTPQMapNode
	for _, queue := range rht.nodeQueueMapByKey {
		for _, value := range queue.Values() {
			nodes = append(nodes, value.(*RHTPQMapNode))
		}
	}

	return nodes
}

// Marshal returns the JSON encoding of this map.
func (rht *RHTPriorityQueueMap) Marshal() string {
	members := rht.Elements()

	size := len(members)

	// Extract and sort the keys
	keys := make([]string, 0, size)
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sb := strings.Builder{}
	sb.WriteString("{")
	for idx, k := range keys {
		if idx > 0 {
			sb.WriteString(",")
		}
		value := members[k]
		sb.WriteString(fmt.Sprintf(`"%s":%s`, k, value.Marshal()))
	}
	sb.WriteString("}")

	return sb.String()
}
