/*
Copyright 2026.

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

package controller

import (
	"context"
	"time"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// FeedFetcher is the port through which the per-feed path fetches a feed. The
// production implementation is *rss.Client, which carries the SSRF guards.
type FeedFetcher interface {
	// FetchEntries fetches and parses the feed at url, using validators for a
	// conditional GET.
	FetchEntries(ctx context.Context, url string, validators rss.CacheValidators) (rss.FetchResult, error)
}

// MessageSender is the port through which the per-feed path delivers a message
// to Discord. The production implementation is *discord.Client, which carries
// the webhook host allowlist.
type MessageSender interface {
	// SendMessage posts msg to the webhook the sender was built for.
	SendMessage(ctx context.Context, msg discord.Message) error
}

var (
	_ FeedFetcher   = (*rss.Client)(nil)
	_ MessageSender = (*discord.Client)(nil)
)

// timedFetcher observes feedRequestDuration for every fetch made for one
// FeedGroup, whether the fetch succeeds or fails.
type timedFetcher struct {
	inner   FeedFetcher
	metrics groupMetrics
}

func newTimedFetcher(inner FeedFetcher, namespace, name string) *timedFetcher {
	return &timedFetcher{inner: inner, metrics: groupMetrics{namespace: namespace, name: name}}
}

func (f *timedFetcher) FetchEntries(ctx context.Context, url string, validators rss.CacheValidators) (rss.FetchResult, error) {
	start := time.Now()
	result, err := f.inner.FetchEntries(ctx, url, validators)
	f.metrics.requestDuration(operationFetch, time.Since(start))
	return result, err
}

// timedSender observes feedRequestDuration for every send made for one
// FeedGroup, whether the send succeeds or fails.
type timedSender struct {
	inner   MessageSender
	metrics groupMetrics
}

func newTimedSender(inner MessageSender, namespace, name string) *timedSender {
	return &timedSender{inner: inner, metrics: groupMetrics{namespace: namespace, name: name}}
}

func (s *timedSender) SendMessage(ctx context.Context, msg discord.Message) error {
	start := time.Now()
	err := s.inner.SendMessage(ctx, msg)
	s.metrics.requestDuration(operationSend, time.Since(start))
	return err
}
