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

package document

import (
	"fmt"

	"github.com/yorkie-team/yorkie/api/converter"
	"github.com/yorkie-team/yorkie/pkg/document/change"
	"github.com/yorkie-team/yorkie/pkg/document/checkpoint"
	"github.com/yorkie-team/yorkie/pkg/document/json"
	"github.com/yorkie-team/yorkie/pkg/document/key"
	"github.com/yorkie-team/yorkie/pkg/document/proxy"
	"github.com/yorkie-team/yorkie/pkg/document/time"
	"github.com/yorkie-team/yorkie/pkg/log"
)

type stateType int

const (
	// Detached means that the document is not attached to the client.
	// The actor of the ticket is created without being assigned.
	Detached stateType = 0

	// Attached means that this document is attached to the client.
	// The actor of the ticket is created with being assigned by the client.
	Attached stateType = 1
)

// Document represents a document in MongoDB and contains logical clocks.
//
// How document works:
// The operations are generated by the proxy while executing user's command on
// the clone. Then the operations will apply the changes into the base json
// root. This is to protect the base json from errors that may occur while user
// edit the document.
type Document struct {
	key          *key.Key
	state        stateType
	root         *json.Root
	clone        *json.Root
	checkpoint   *checkpoint.Checkpoint
	changeID     *change.ID
	localChanges []*change.Change
}

// New creates a new instance of Document.
func New(collection, document string) *Document {
	root := json.NewObject(json.NewRHT(), time.InitialTicket)

	return &Document{
		key:        &key.Key{Collection: collection, Document: document},
		state:      Detached,
		root:       json.NewRoot(root),
		checkpoint: checkpoint.Initial,
		changeID:   change.InitialID,
	}
}

// New creates a new instance of Document with the snapshot.
func FromSnapshot(
	collection string,
	document string,
	serverSeq uint64,
	snapshot []byte,
) (*Document, error) {
	obj, err := converter.BytesToObject(snapshot)
	if err != nil {
		return nil, err
	}

	return &Document{
		key:        &key.Key{Collection: collection, Document: document},
		state:      Detached,
		root:       json.NewRoot(obj),
		checkpoint: checkpoint.Initial.NextServerSeq(serverSeq),
		changeID:   change.InitialID,
	}, nil
}

// Key returns the key of this document.
func (d *Document) Key() *key.Key {
	return d.key
}

// Checkpoint returns the checkpoint of this document.
func (d *Document) Checkpoint() *checkpoint.Checkpoint {
	return d.checkpoint
}

// Update executes the given updater to update this document.
func (d *Document) Update(
	updater func(root *proxy.ObjectProxy) error,
	msgAndArgs ...interface{},
) error {
	d.ensureClone()
	ctx := change.NewContext(
		d.changeID.Next(),
		messageFromMsgAndArgs(msgAndArgs),
		d.clone,
	)

	if err := updater(proxy.NewObjectProxy(ctx, d.clone.Object())); err != nil {
		// drop clone because it is contaminated.
		d.clone = nil
		log.Logger.Error(err)
		return err
	}

	if ctx.HasOperations() {
		c := ctx.ToChange()
		if err := c.Execute(d.root); err != nil {
			return err
		}

		d.localChanges = append(d.localChanges, c)
		d.changeID = ctx.ID()
	}

	return nil
}

// HasLocalChanges returns whether this document has local changes or not.
func (d *Document) HasLocalChanges() bool {
	return len(d.localChanges) > 0
}

// ApplyChangePack applies the given change pack into this document.
func (d *Document) ApplyChangePack(pack *change.Pack) error {
	// 01. Apply remote changes to both the clone and the document.
	if len(pack.Snapshot) > 0 {
		if err := d.applySnapshot(pack.Snapshot, pack.Checkpoint.ServerSeq); err != nil {
			return err
		}
	} else {
		if err := d.applyChanges(pack.Changes); err != nil {
			return err
		}
	}

	// 02. Remove local changes applied to server.
	for d.HasLocalChanges() {
		c := d.localChanges[0]
		if c.ClientSeq() > pack.Checkpoint.ClientSeq {
			break
		}
		d.localChanges = d.localChanges[1:]
	}

	// 03. Update the checkpoint.
	d.checkpoint = d.checkpoint.Forward(pack.Checkpoint)

	log.Logger.Debugf("after apply %d changes: %s", len(pack.Changes), d.RootObject().Marshal())
	return nil
}

func (d *Document) applySnapshot(snapshot []byte, serverSeq uint64) error {
	rootObj, err := converter.BytesToObject(snapshot)
	if err != nil {
		return err
	}
	d.root = json.NewRoot(rootObj)

	if d.HasLocalChanges() {
		for _, c := range d.localChanges {
			if err := c.Execute(d.root); err != nil {
				return err
			}
		}
	}
	d.changeID = d.changeID.SyncLamport(serverSeq)

	// drop clone because it is contaminated.
	d.clone = nil

	return nil
}

// applyChanges applies remote changes to both the clone and the document.
func (d *Document) applyChanges(changes []*change.Change) error {
	d.ensureClone()

	for _, c := range changes {
		if err := c.Execute(d.clone); err != nil {
			return err
		}
	}

	for _, c := range changes {
		if err := c.Execute(d.root); err != nil {
			return err
		}
		d.changeID = d.changeID.SyncLamport(c.ID().Lamport())
	}

	return nil
}

// Marshal returns the JSON encoding of this document.
func (d *Document) Marshal() string {
	return d.root.Object().Marshal()
}

// CreateChangePack creates pack of the local changes to send to the server.
func (d *Document) CreateChangePack() *change.Pack {
	changes := d.localChanges

	cp := d.checkpoint.IncreaseClientSeq(uint32(len(changes)))
	return change.NewPack(d.key, cp, changes, nil)
}

// SetActor sets actor into this document. This is also applied in the local
// changes the document has.
func (d *Document) SetActor(actor *time.ActorID) {
	for _, c := range d.localChanges {
		c.SetActor(actor)
	}
	d.changeID = d.changeID.SetActor(actor)
}

// Actor sets actor.
func (d *Document) Actor() *time.ActorID {
	return d.changeID.Actor()
}

// UpdateState updates the state of this document.
func (d *Document) UpdateState(state stateType) {
	d.state = state
}

// IsAttached returns the whether this document is attached or not.
func (d *Document) IsAttached() bool {
	return d.state == Attached
}

func (d *Document) ensureClone() {
	if d.clone == nil {
		d.clone = d.root.DeepCopy()
	}
}

func (d *Document) RootObject() *json.Object {
	return d.root.Object()
}

func messageFromMsgAndArgs(msgAndArgs ...interface{}) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	if len(msgAndArgs) == 1 {
		msg := msgAndArgs[0]
		if msgAsStr, ok := msg.(string); ok {
			return msgAsStr
		}
		return fmt.Sprintf("%+v", msg)
	}
	if len(msgAndArgs) > 1 {
		return fmt.Sprintf(msgAndArgs[0].(string), msgAndArgs[1:]...)
	}
	return ""
}
