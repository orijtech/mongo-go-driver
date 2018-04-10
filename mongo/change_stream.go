// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongo

import (
	"bytes"
	"context"
	"errors"

	"github.com/mongodb/mongo-go-driver/bson"
	"github.com/mongodb/mongo-go-driver/core/command"
	"github.com/mongodb/mongo-go-driver/core/options"
	"github.com/mongodb/mongo-go-driver/internal/trace"
)

// ErrMissingResumeToken indicates that a change stream notification from the server did not
// contain a resume token.
var ErrMissingResumeToken = errors.New("cannot provide resume functionality when the resume token is missing")

type changeStream struct {
	pipeline    *bson.Array
	options     []options.ChangeStreamOptioner
	coll        *Collection
	cursor      Cursor
	resumeToken *bson.Document
	err         error
}

const errorCodeNotMaster int32 = 10107
const errorCodeCursorNotFound int32 = 43

func newChangeStream(ctx context.Context, coll *Collection, pipeline interface{},
	opts ...options.ChangeStreamOptioner) (*changeStream, error) {

	ctx, span := trace.SpanFromFunctionCaller(ctx)
	defer span.End()

	trace.AnnotateStrings(span, "New changes stream", nil)

	pipelineArr, err := transformAggregatePipeline(pipeline)
	if err != nil {
		return nil, err
	}

	changeStreamOptions := bson.NewDocument()

	for _, opt := range opts {
		opt.Option(changeStreamOptions)
	}

	pipelineArr.Prepend(
		bson.VC.Document(
			bson.NewDocument(
				bson.EC.SubDocument("$changeStream", changeStreamOptions))))

	trace.AnnotateStrings(span, "Aggregation the collection", nil)
	cursor, err := coll.Aggregate(ctx, pipelineArr)
	if err != nil {
		return nil, err
	}

	cs := &changeStream{
		pipeline: pipelineArr,
		options:  opts,
		coll:     coll,
		cursor:   cursor,
	}

	return cs, nil
}

func (cs *changeStream) ID() int64 {
	return cs.cursor.ID()
}

func (cs *changeStream) Next(ctx context.Context) bool {
	ctx, span := trace.SpanFromFunctionCaller(ctx)
	defer span.End()

	trace.AnnotateStrings(span, "Next", nil)

	if cs.cursor.Next(ctx) {
		return true
	}

	err := cs.cursor.Err()
	if err == nil {
		return false
	}

	switch t := err.(type) {
	case command.Error:
		if t.Code != errorCodeNotMaster && t.Code != errorCodeCursorNotFound {
			return false
		}
	}

	resumeToken := Opt.ResumeAfter(cs.resumeToken)
	found := false

	for i, opt := range cs.options {
		if _, ok := opt.(options.OptResumeAfter); ok {
			cs.options[i] = resumeToken
			found = true
			break
		}
	}

	if !found {
		cs.options = append(cs.options, resumeToken)
	}

	oldns := cs.coll.namespace()
	killCursors := command.KillCursors{
		NS:  command.Namespace{DB: oldns.DB, Collection: oldns.Collection},
		IDs: []int64{cs.ID()},
	}

	trace.AnnotateStrings(span, "Selecting the server in the topology", nil)
	ss, err := cs.coll.client.topology.SelectServer(ctx, cs.coll.readSelector)
	if err != nil {
		cs.err = err
		return false
	}

	trace.AnnotateStrings(span, "Now retrieving the connection", nil)
	conn, err := ss.Connection(ctx)
	if err != nil {
		cs.err = err
		return false
	}
	defer conn.Close()

	_, _ = killCursors.RoundTrip(ctx, ss.Description(), conn)

	changeStreamOptions := bson.NewDocument()

	for _, opt := range cs.options {
		opt.Option(changeStreamOptions)
	}

	cs.pipeline.Set(0, bson.VC.Document(
		bson.NewDocument(
			bson.EC.SubDocument("$changeStream", changeStreamOptions)),
	),
	)

	oldns = cs.coll.namespace()
	aggCmd := command.Aggregate{
		NS:       command.Namespace{DB: oldns.DB, Collection: oldns.Collection},
		Pipeline: cs.pipeline,
	}
	trace.AnnotateStrings(span, "Now invoking aggregate command RoundTrip", nil)
	cs.cursor, cs.err = aggCmd.RoundTrip(ctx, ss.Description(), ss, conn)

	if cs.err != nil {
		return false
	}

	return cs.cursor.Next(ctx)
}

func (cs *changeStream) Decode(out interface{}) error {
	br, err := cs.DecodeBytes()
	if err != nil {
		return err
	}

	return bson.NewDecoder(bytes.NewReader(br)).Decode(out)
}

func (cs *changeStream) DecodeBytes() (bson.Reader, error) {
	br, err := cs.cursor.DecodeBytes()
	if err != nil {
		return nil, err
	}

	id, err := br.Lookup("_id")
	if err != nil {
		_ = cs.Close(context.Background())
		return nil, ErrMissingResumeToken
	}

	cs.resumeToken = id.Value().MutableDocument()

	return br, nil
}

func (cs *changeStream) Err() error {
	if cs.err != nil {
		return cs.err
	}

	return cs.cursor.Err()
}

func (cs *changeStream) Close(ctx context.Context) error {
	return cs.cursor.Close(ctx)
}
