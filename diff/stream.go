/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package diff

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	handlers []Handler

	// ErrNoProcessor is returned when no stream processor is avaliable for a media-type
	ErrNoProcessor = errors.New("no processor for media-type")
)

func init() {
	// register the default compression handler
	RegisterProcessor(compressedHandler)
}

// RegisterProcessor registers a stream processor for media-types
func RegisterProcessor(handler Handler) {
	handlers = append(handlers, handler)
}

// GetProcessor returns the processor for a media-type
func GetProcessor(ctx context.Context, mediaType string, stream StreamProcessor, payloads map[string]interface{}) (StreamProcessor, error) {
	// reverse this list so that user configured handlers come up first
	for i := len(handlers); i >= 0; i-- {
		processor, ok := handlers[i](ctx, mediaType)
		if ok {
			return processor(ctx, stream, payloads)
		}
	}
	return nil, ErrNoProcessor
}

// Handler checks a media-type and initializes the processor
type Handler func(ctx context.Context, mediaType string) (StreamProcessorInit, bool)

// StaticHandler returns the processor init func for a static media-type
func StaticHandler(expectedMediaType string, fn StreamProcessorInit) Handler {
	return func(ctx context.Context, mediaType string) (StreamProcessorInit, bool) {
		if mediaType == expectedMediaType {
			return fn, true
		}
		return nil, false
	}
}

// StreamProcessorInit returns the initialized stream processor
type StreamProcessorInit func(ctx context.Context, stream StreamProcessor, payloads map[string]interface{}) (StreamProcessor, error)

// RawProcessor provides access to direct fd for processing
type RawProcessor interface {
	// File returns the fd for the read stream of the underlying processor
	File() *os.File
}

// StreamProcessor handles processing a content stream and transforming it into a different media-type
type StreamProcessor interface {
	io.ReadCloser

	// MediaType is the resulting media-type that the processor processes the stream into
	MediaType() string
}

func compressedHandler(ctx context.Context, mediaType string) (StreamProcessorInit, bool) {
	compressed, err := images.IsCompressedDiff(ctx, mediaType)
	if err != nil {
		return nil, false
	}
	if compressed {
		return func(ctx context.Context, stream StreamProcessor, payloads map[string]interface{}) (StreamProcessor, error) {
			ds, err := compression.DecompressStream(stream)
			if err != nil {
				return nil, err
			}

			return &compressedProcessor{
				rc: ds,
			}, nil
		}, true
	}
	return func(ctx context.Context, stream StreamProcessor, payloads map[string]interface{}) (StreamProcessor, error) {
		return &stdProcessor{
			rc: stream,
		}, nil
	}, true
}

// NewProcessorChain initialized the root StreamProcessor
func NewProcessorChain(mt string, r io.Reader) StreamProcessor {
	return &processorChain{
		mt: mt,
		rc: r,
	}
}

type processorChain struct {
	mt string
	rc io.Reader
}

func (c *processorChain) MediaType() string {
	return c.mt
}

func (c *processorChain) Read(p []byte) (int, error) {
	return c.rc.Read(p)
}

func (c *processorChain) Close() error {
	return nil
}

type stdProcessor struct {
	rc StreamProcessor
}

func (c *stdProcessor) MediaType() string {
	return ocispec.MediaTypeImageLayer
}

func (c *stdProcessor) Read(p []byte) (int, error) {
	return c.rc.Read(p)
}

func (c *stdProcessor) Close() error {
	return nil
}

type compressedProcessor struct {
	rc io.ReadCloser
}

func (c *compressedProcessor) MediaType() string {
	return ocispec.MediaTypeImageLayer
}

func (c *compressedProcessor) Read(p []byte) (int, error) {
	return c.rc.Read(p)
}

func (c *compressedProcessor) Close() error {
	return c.rc.Close()
}

// NewBinaryProcessor returns a binary processor for use with processing content streams
func NewBinaryProcessor(ctx context.Context, mt string, stream StreamProcessor, name string, args ...string) (StreamProcessor, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var (
		stdin *os.File
		err   error
	)
	if f, ok := stream.(RawProcessor); ok {
		stdin = f.File()
	} else {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		stdin = r
		go func() {
			io.Copy(w, stream)
		}()
	}
	cmd.Stdin = stdin
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// close after start and dup
	stdin.Close()
	w.Close()

	return &binaryProcessor{
		cmd: cmd,
		r:   r,
		mt:  mt,
	}, nil
}

type binaryProcessor struct {
	cmd *exec.Cmd
	r   *os.File
	mt  string
}

func (c *binaryProcessor) File() *os.File {
	return c.r
}

func (c *binaryProcessor) MediaType() string {
	return c.mt
}

func (c *binaryProcessor) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *binaryProcessor) Close() error {
	err := c.r.Close()
	if kerr := c.cmd.Process.Kill(); err == nil {
		err = kerr
	}
	return err
}
