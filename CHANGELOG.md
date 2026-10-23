# Changelog

## [0.13.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.13.0...v0.13.1) (2026-10-23)


### Bug Fixes

* **container:** update image gcr.io/distroless/static (5167571 → d620678) ([#277](https://github.com/maverickd650/rss2discord-operator/issues/277)) ([fc766ff](https://github.com/maverickd650/rss2discord-operator/commit/fc766ff1c2999b02ddd846cee95ead2f3a217c03))
* **container:** update image gcr.io/distroless/static (e2e927e → 5167571) ([#272](https://github.com/maverickd650/rss2discord-operator/issues/272)) ([fdd352b](https://github.com/maverickd650/rss2discord-operator/commit/fdd352b375c5a55aaf8ed93c15bdd72051df6789))
* **deps:** update dependency promtool (3.15.0 → 3.15.1) ([#279](https://github.com/maverickd650/rss2discord-operator/issues/279)) ([9776cbc](https://github.com/maverickd650/rss2discord-operator/commit/9776cbc2bf4b9e0bcfad6d004f9dfd8074c4bd99))
* **go:** Update Go toolchain ([#273](https://github.com/maverickd650/rss2discord-operator/issues/273)) ([a4c880a](https://github.com/maverickd650/rss2discord-operator/commit/a4c880aac104879779d5a18219592ed682e86210))
* **go:** Update image golang (9578611 → f241642) ([#276](https://github.com/maverickd650/rss2discord-operator/issues/276)) ([53b7152](https://github.com/maverickd650/rss2discord-operator/commit/53b7152906b798f62f4925b18d9d12f58eac3cb4))
* **go:** Update image golang (f241642 → f20d83a) ([#278](https://github.com/maverickd650/rss2discord-operator/issues/278)) ([74c6c67](https://github.com/maverickd650/rss2discord-operator/commit/74c6c678a351e842501fa5f7a1e1eff1acb6ec0d))

## [0.13.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.12.2...v0.13.0) (2026-10-17)


### Features

* **deps:** update module go:sigs.k8s.io/controller-tools/cmd/controller-gen (0.22.0 → v0.23.0) ([#267](https://github.com/maverickd650/rss2discord-operator/issues/267)) ([100abbb](https://github.com/maverickd650/rss2discord-operator/commit/100abbbe2fbb3ba6e3064355274cf628c8ad4013))
* **deps:** update module sigs.k8s.io/logtools (v0.10.1 → v0.11.0) ([#269](https://github.com/maverickd650/rss2discord-operator/issues/269)) ([8375eaf](https://github.com/maverickd650/rss2discord-operator/commit/8375eaf181ed81a0c680ed229a6b95c78b1978fd))


### Bug Fixes

* **go:** Update image golang (007a316 → 8201530) ([#265](https://github.com/maverickd650/rss2discord-operator/issues/265)) ([9f40563](https://github.com/maverickd650/rss2discord-operator/commit/9f405632bfb4f912f8c8218eb0c7efae5cdb024b))
* **go:** Update image golang (e432b43 → 007a316) ([#263](https://github.com/maverickd650/rss2discord-operator/issues/263)) ([35966b1](https://github.com/maverickd650/rss2discord-operator/commit/35966b1502aeb0d68e37a359b8b8033464db7327))

## [0.12.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.12.1...v0.12.2) (2026-10-10)


### Bug Fixes

* fix vulernability in cel ([#249](https://github.com/maverickd650/rss2discord-operator/issues/249)) ([05418bd](https://github.com/maverickd650/rss2discord-operator/commit/05418bd7cf10c0d5066cb1182ffe0009bcc7ffd6))
* redo strip html ([#251](https://github.com/maverickd650/rss2discord-operator/issues/251)) ([08977d4](https://github.com/maverickd650/rss2discord-operator/commit/08977d4442e3c6bcf672070a769759a2528ef785))
* stop self-triggered reconcile loop, cap permanent backoff, pace Discord sends, bound status size ([#258](https://github.com/maverickd650/rss2discord-operator/issues/258)) ([0ab03b2](https://github.com/maverickd650/rss2discord-operator/commit/0ab03b242710c49b951468e75423af1b0146952d))
* stop webhook token leaking via parse errors; harden span redaction ([#257](https://github.com/maverickd650/rss2discord-operator/issues/257)) ([37fda3f](https://github.com/maverickd650/rss2discord-operator/commit/37fda3fb6a6ac78e6ac77bd35707d7e0c1bfba21))


### Performance Improvements

* **rss:** avoid failed time.Parse attempts in parseTime ([#252](https://github.com/maverickd650/rss2discord-operator/issues/252)) ([5f63b56](https://github.com/maverickd650/rss2discord-operator/commit/5f63b56287b6a41efe626989493b60eb961a52e2))

## [0.12.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.12.0...v0.12.1) (2026-10-09)


### Bug Fixes

* **go:** Update Go toolchain ([#246](https://github.com/maverickd650/rss2discord-operator/issues/246)) ([d75a857](https://github.com/maverickd650/rss2discord-operator/commit/d75a85754446962d9a957f89619e458e4340f16a))
* **go:** Update image golang (1e93e00 → 162be52) ([#242](https://github.com/maverickd650/rss2discord-operator/issues/242)) ([bd7ffc9](https://github.com/maverickd650/rss2discord-operator/commit/bd7ffc9765a2f0288ac1974e915ecea820b3766d))

## [0.12.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.11.2...v0.12.0) (2026-10-06)


### Features

* **container:** update image golang (1.26 → 1.27) ([#194](https://github.com/maverickd650/rss2discord-operator/issues/194)) ([8adee4d](https://github.com/maverickd650/rss2discord-operator/commit/8adee4d78ca391f748073860e8ec61201c0137b7))
* **deps:** update dependency golangci-lint (2.12.2 → 2.13.1) ([#195](https://github.com/maverickd650/rss2discord-operator/issues/195)) ([8450ad9](https://github.com/maverickd650/rss2discord-operator/commit/8450ad979b879ac26cf5cb23883e25a191bcb9b6))
* **deps:** update dependency golangci-lint (2.13.1 → 2.14.0) ([#202](https://github.com/maverickd650/rss2discord-operator/issues/202)) ([0a31949](https://github.com/maverickd650/rss2discord-operator/commit/0a31949ed7350945d602225628bcad1be2867319))
* **deps:** update dependency kubebuilder (4.15.0 → 4.16.0) ([#222](https://github.com/maverickd650/rss2discord-operator/issues/222)) ([4f95c22](https://github.com/maverickd650/rss2discord-operator/commit/4f95c22618df3e54f88d3ae7d00dbc70cf0194f0))
* **deps:** update dependency kubectl (1.36.2 → 1.37.0) ([#166](https://github.com/maverickd650/rss2discord-operator/issues/166)) ([d0909db](https://github.com/maverickd650/rss2discord-operator/commit/d0909dbc6e4520801987c81b4ea2a09275f9ce31))
* **deps:** update dependency promtool (3.13.1 → 3.14.0) ([#178](https://github.com/maverickd650/rss2discord-operator/issues/178)) ([f10fef8](https://github.com/maverickd650/rss2discord-operator/commit/f10fef8c1908aee2767ade04eb11c891ab07d166))
* **deps:** update dependency promtool (3.14.0 → 3.15.0) ([#224](https://github.com/maverickd650/rss2discord-operator/issues/224)) ([99a3ca1](https://github.com/maverickd650/rss2discord-operator/commit/99a3ca16955add30e21a70567e07ca5947f53ced))
* **deps:** update dependency setup-envtest (0.24.1 → 0.25.2) ([#211](https://github.com/maverickd650/rss2discord-operator/issues/211)) ([052fc9d](https://github.com/maverickd650/rss2discord-operator/commit/052fc9d7097c0a11cee0f7977d6f832f84d1100a))
* upgrade kubebuilder scaffold to v4.16.0 ([#236](https://github.com/maverickd650/rss2discord-operator/issues/236)) ([1046cbd](https://github.com/maverickd650/rss2discord-operator/commit/1046cbdd8e96370596c813d83db7d53d62713d6c))


### Bug Fixes

* build with Go 1.27.1 and write status via generated SSA apply configurations ([#237](https://github.com/maverickd650/rss2discord-operator/issues/237)) ([ab467ec](https://github.com/maverickd650/rss2discord-operator/commit/ab467ec897e418fc8e1c393f70218274d2398ca1))
* **container:** update image gcr.io/distroless/static (1c2c046 → e2e927e) ([#212](https://github.com/maverickd650/rss2discord-operator/issues/212)) ([469546e](https://github.com/maverickd650/rss2discord-operator/commit/469546e67cf9193d2c13baf80d5efa6375f1662e))
* **container:** update image gcr.io/distroless/static (d29e660 → f7f8f72) ([#157](https://github.com/maverickd650/rss2discord-operator/issues/157)) ([f0f1a3d](https://github.com/maverickd650/rss2discord-operator/commit/f0f1a3d9f383463db9c3479ecf33c37939a8f4ee))
* **container:** update image gcr.io/distroless/static (f7f8f72 → 1c2c046) ([#196](https://github.com/maverickd650/rss2discord-operator/issues/196)) ([c9d4f03](https://github.com/maverickd650/rss2discord-operator/commit/c9d4f036204f1c46399f646ca85054480ebcb78e))
* **container:** update image golang (079e598 → d52df9c) ([#156](https://github.com/maverickd650/rss2discord-operator/issues/156)) ([902a1b3](https://github.com/maverickd650/rss2discord-operator/commit/902a1b37b6d7cab6880b3579f9855438b3ea45e5))
* **container:** update image golang (0ecdc2a → 23fe807) ([#207](https://github.com/maverickd650/rss2discord-operator/issues/207)) ([85e8de9](https://github.com/maverickd650/rss2discord-operator/commit/85e8de94e4881c7747d0c879f1019e43c0ba81be))
* **container:** update image golang (23fe807 → 1e93e00) ([#235](https://github.com/maverickd650/rss2discord-operator/issues/235)) ([3e19629](https://github.com/maverickd650/rss2discord-operator/commit/3e196293082ab14e4c15e99c6e385539bbc9e95f))
* **container:** update image golang (ae5a231 → 3aff665) ([#163](https://github.com/maverickd650/rss2discord-operator/issues/163)) ([2976114](https://github.com/maverickd650/rss2discord-operator/commit/2976114e333bb7521ca00833184a5ee73160f70b))
* **container:** update image golang (d52df9c → ae5a231) ([#158](https://github.com/maverickd650/rss2discord-operator/issues/158)) ([2960d34](https://github.com/maverickd650/rss2discord-operator/commit/2960d343f944384143aa9fa256780c9483b6026f))
* **deps:** update dependency kubectl (1.37.0 → 1.37.1) ([#223](https://github.com/maverickd650/rss2discord-operator/issues/223)) ([cc75e15](https://github.com/maverickd650/rss2discord-operator/commit/cc75e15a6a95a4b71a5744a0457959efa19572cf))
* **deps:** update dependency promtool (3.13.0 → 3.13.1) ([#152](https://github.com/maverickd650/rss2discord-operator/issues/152)) ([a7ce8a2](https://github.com/maverickd650/rss2discord-operator/commit/a7ce8a2b3d81d6c0f9b3f8e7f836c3c26ac9a651))
* **rss:** enforce SSRF guard at connect time and harden transport ([#240](https://github.com/maverickd650/rss2discord-operator/issues/240)) ([4089031](https://github.com/maverickd650/rss2discord-operator/commit/40890315c7e893243ede41142b0a7611746fdf90))


### Performance Improvements

* set GOMEMLIMIT, cache-friendly image build, prune removed feed metrics ([#241](https://github.com/maverickd650/rss2discord-operator/issues/241)) ([19b5ae3](https://github.com/maverickd650/rss2discord-operator/commit/19b5ae307365eba34ebe4eddb22a54463a01bd7d))

## [0.11.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.11.1...v0.11.2) (2026-07-09)


### Bug Fixes

* **container:** update image gcr.io/distroless/static (963fa6c → d29e660) ([#146](https://github.com/maverickd650/rss2discord-operator/issues/146)) ([2733cda](https://github.com/maverickd650/rss2discord-operator/commit/2733cda584e4cc3ae72cd97b9d24e9c357237eaf))
* **container:** update image golang (b900de9 → 079e598) ([#147](https://github.com/maverickd650/rss2discord-operator/issues/147)) ([25ca43b](https://github.com/maverickd650/rss2discord-operator/commit/25ca43b132aa5a8da8ce089ebc966b8bedc96f52))

## [0.11.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.11.0...v0.11.1) (2026-07-08)


### Bug Fixes

* **container:** update image golang (f96cc55 → b900de9) ([#141](https://github.com/maverickd650/rss2discord-operator/issues/141)) ([dadc408](https://github.com/maverickd650/rss2discord-operator/commit/dadc408b62116f758d3f578cb4e48e603300ac47))
* **deps:** update dependency go (1.26.4 → 1.26.5) ([#142](https://github.com/maverickd650/rss2discord-operator/issues/142)) ([5a27be2](https://github.com/maverickd650/rss2discord-operator/commit/5a27be25f2a35168299ef774bfcefb386efe3722))

## [0.11.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.10.1...v0.11.0) (2026-07-03)


### Features

* add optional OTel tracing on outbound RSS/Discord HTTP ([#115](https://github.com/maverickd650/rss2discord-operator/issues/115)) ([2f47a64](https://github.com/maverickd650/rss2discord-operator/commit/2f47a640e6655d52cd08dc85d0c809984cdb1868))
* **controller:** add requeue jitter and pin the priority queue ([#110](https://github.com/maverickd650/rss2discord-operator/issues/110)) ([607bb4f](https://github.com/maverickd650/rss2discord-operator/commit/607bb4f37f31917e5073c81439b3b07f4cd24c3c))
* **controller:** migrate status writes to server-side apply ([#112](https://github.com/maverickd650/rss2discord-operator/issues/112)) ([5e21247](https://github.com/maverickd650/rss2discord-operator/commit/5e212478846f708e6c7e859991d456a04b7e6000))
* **discord:** share a process-wide rate limiter across webhook clients ([#109](https://github.com/maverickd650/rss2discord-operator/issues/109)) ([8d8683b](https://github.com/maverickd650/rss2discord-operator/commit/8d8683be2033b0dd1e6f5cf15de98abcab974245))
* **rss:** support RSS 1.0 (RDF) feeds and classify unrecognized formats ([#107](https://github.com/maverickd650/rss2discord-operator/issues/107)) ([71ebbef](https://github.com/maverickd650/rss2discord-operator/commit/71ebbef3dc246f60589f331af29dcf78ba853da6))


### Bug Fixes

* clean up resurrected test-chart.yml in helm-chart-refresh ([#134](https://github.com/maverickd650/rss2discord-operator/issues/134)) ([7dacbd4](https://github.com/maverickd650/rss2discord-operator/commit/7dacbd46882c3c50bc1f9a5032f70117ce481fc5))
* **devcontainer:** install mise from a pinned release binary, not curl|sh ([#118](https://github.com/maverickd650/rss2discord-operator/issues/118)) ([cf23976](https://github.com/maverickd650/rss2discord-operator/commit/cf23976be346a9a112e439c166ee6e890de849c5))
* sync dist/install.yaml with the current CRD ([#130](https://github.com/maverickd650/rss2discord-operator/issues/130)) ([2b1773d](https://github.com/maverickd650/rss2discord-operator/commit/2b1773dbdeeb452650a2104a170fc857ddb6d6b0))

## [0.10.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.10.0...v0.10.1) (2026-07-01)


### Bug Fixes

* harden reconcile interval, SSRF guard, and Discord redirects ([#99](https://github.com/maverickd650/rss2discord-operator/issues/99)) ([a6ffec3](https://github.com/maverickd650/rss2discord-operator/commit/a6ffec36e3f695a7801fc385ee4d1e080a02d452))

## [0.10.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.9.1...v0.10.0) (2026-06-27)


### Features

* **api:** harden FeedGroup CRD validation and field types ([#93](https://github.com/maverickd650/rss2discord-operator/issues/93)) ([605ed9e](https://github.com/maverickd650/rss2discord-operator/commit/605ed9ee85ad96bff8a7f5993b5d8bd1958c0d1f))

## [0.9.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.9.0...v0.9.1) (2026-06-27)


### Bug Fixes

* **deps:** bump go.opentelemetry.io/otel/sdk to v1.44.0 ([#85](https://github.com/maverickd650/rss2discord-operator/issues/85)) ([e86dbdf](https://github.com/maverickd650/rss2discord-operator/commit/e86dbdf74b3d6ca4cb233348daad286876b28bea))
* **rss:** close NAT64 (RFC 6052) SSRF guard bypass in isPublicIP ([#90](https://github.com/maverickd650/rss2discord-operator/issues/90)) ([868833c](https://github.com/maverickd650/rss2discord-operator/commit/868833ce38f668caf2fbe79e76dd7d07f6c1ad66))

## [0.9.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.8.0...v0.9.0) (2026-06-26)


### Features

* add mise task to refresh dist/chart from config/ without clobbering hand edits ([#81](https://github.com/maverickd650/rss2discord-operator/issues/81)) ([38c4c88](https://github.com/maverickd650/rss2discord-operator/commit/38c4c888956eb32b3257643059cd1f28b20ef70a))
* identify the failing feed and reason in fetch/send-error alerts ([#83](https://github.com/maverickd650/rss2discord-operator/issues/83)) ([b8ada6c](https://github.com/maverickd650/rss2discord-operator/commit/b8ada6c61c1d8c9b5b5de1aefc168ef640d08fc1))

## [0.8.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.7.0...v0.8.0) (2026-06-26)


### ⚠ BREAKING CHANGES

* a feed entry that permanently fails to send (e.g. Discord 400s on that entry's specific content) now blocks every later entry in the same feed until it's resolved or the FeedGroup spec changes, instead of being silently dropped while the rest of the feed kept flowing. This trades head-of-line blocking for no-silent-data-loss, which we consider the correct default for an at-least-once delivery system, but it is an observable behavior change worth a release note.

### Bug Fixes

* redact webhook secrets from errors, clear stale failure state on recovery ([#79](https://github.com/maverickd650/rss2discord-operator/issues/79)) ([5be7d6b](https://github.com/maverickd650/rss2discord-operator/commit/5be7d6bb1f3dcd0682dccfbc1f045e0e6e1dd6ba))
* stop a failed feed entry from being silently dropped or stranded ([#77](https://github.com/maverickd650/rss2discord-operator/issues/77)) ([2fed502](https://github.com/maverickd650/rss2discord-operator/commit/2fed50298c358c9efa9401d1da2321122a7ff64a))

## [0.7.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.6.3...v0.7.0) (2026-06-25)


### ⚠ BREAKING CHANGES

* rss2discord_feed_request_duration_seconds and rss2discord_message_overflow_chars no longer expose classic _bucket series. Any external dashboard, recording rule, or alert querying ..._bucket/le on these two metrics will stop matching. Scraping with prometheus.scrapeNativeHistograms=false (or an older Prometheus without native histogram support) now yields _count/_sum only, with no histogram_quantile resolution -- there is no classic fallback. The bundled dashboard and PrometheusRule have been updated accordingly.
* FeedGroupStatus's per-feed-URL maps (lastChecked, lastSeenEntry, lastSent, lastError, feedETag, feedLastModified, retryCount) are replaced by status.feeds[], keyed by rssUrl. Status is server-generated, so existing FeedGroups get fresh status on their next reconcile with no manual migration -- but anything scripting against the old shape (e.g. `kubectl get feedgroup -o jsonpath='{.status.lastError}'`) needs to read .status.feeds[].lastError instead.
* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (3 → 4) ([#70](https://github.com/maverickd650/rss2discord-operator/issues/70))

### Features

* add exponential backoff for permanent feed fetch failures ([#75](https://github.com/maverickd650/rss2discord-operator/issues/75)) ([fc3c804](https://github.com/maverickd650/rss2discord-operator/commit/fc3c8040cff366df295b0a1c3bdfcf2acf9d7e3d))
* classify feed failures, restructure status, and diversify metrics ([#73](https://github.com/maverickd650/rss2discord-operator/issues/73)) ([3a1fb6c](https://github.com/maverickd650/rss2discord-operator/commit/3a1fb6c292fd9a07a08dee81a9e85f5fd645b349))
* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (3 → 4) ([#70](https://github.com/maverickd650/rss2discord-operator/issues/70)) ([68a998d](https://github.com/maverickd650/rss2discord-operator/commit/68a998d852fd950cd1012cecc429fe247f33f670))
* export histograms native-only and add reconcile duration metric ([#76](https://github.com/maverickd650/rss2discord-operator/issues/76)) ([b370386](https://github.com/maverickd650/rss2discord-operator/commit/b3703862bfd5522424e41736f342e2f3dbc94015))

## [0.6.3](https://github.com/maverickd650/rss2discord-operator/compare/v0.6.2...v0.6.3) (2026-06-24)


### Bug Fixes

* add capabilities ([#65](https://github.com/maverickd650/rss2discord-operator/issues/65)) ([e369b02](https://github.com/maverickd650/rss2discord-operator/commit/e369b0204e8819720b027e71cc80f209d7b7448c))

## [0.6.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.6.1...v0.6.2) (2026-06-23)


### Bug Fixes

* freshness tracks last successful check, not last delivery ([#63](https://github.com/maverickd650/rss2discord-operator/issues/63)) ([f03142f](https://github.com/maverickd650/rss2discord-operator/commit/f03142fe80e270e2a65215e02ff5ceba997f4e1d))

## [0.6.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.6.0...v0.6.1) (2026-06-23)


### Bug Fixes

* **chart:** move ServiceMonitor native histogram fields to spec top level ([#61](https://github.com/maverickd650/rss2discord-operator/issues/61)) ([a8cb30b](https://github.com/maverickd650/rss2discord-operator/commit/a8cb30b18c4beed468c1d74648739dfab57b60f9))

## [0.6.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.5.0...v0.6.0) (2026-06-23)


### Features

* add Prometheus native histogram support for request latency ([#58](https://github.com/maverickd650/rss2discord-operator/issues/58)) ([dec2ffb](https://github.com/maverickd650/rss2discord-operator/commit/dec2ffb177ec7670e78d27a36897ba6834d7989d))
* redesign Grafana dashboard and ship it via grafana-operator ([#57](https://github.com/maverickd650/rss2discord-operator/issues/57)) ([a0e69c1](https://github.com/maverickd650/rss2discord-operator/commit/a0e69c10b1ef57505da29ec430a7619e96bb05ec))


### Bug Fixes

* increase test coverage ([#54](https://github.com/maverickd650/rss2discord-operator/issues/54)) ([c6c4cd0](https://github.com/maverickd650/rss2discord-operator/commit/c6c4cd03a692c84102b176338f37906d4c2780aa))

## [0.5.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.3...v0.5.0) (2026-06-23)


### Features

* add request latency and delivery freshness metrics ([#50](https://github.com/maverickd650/rss2discord-operator/issues/50)) ([503d475](https://github.com/maverickd650/rss2discord-operator/commit/503d47565617ede56bd3e2ffd85a3a2121578baa))


### Bug Fixes

* deliver newest entries for date-less feeds and harden rate-limit/status handling ([#48](https://github.com/maverickd650/rss2discord-operator/issues/48)) ([8adaa4f](https://github.com/maverickd650/rss2discord-operator/commit/8adaa4f967a18b11ac474246558a8e68d008f18b))

## [0.4.3](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.2...v0.4.3) (2026-06-22)


### Bug Fixes

* flag the LastChecked 304 status-write fix for release-please ([#45](https://github.com/maverickd650/rss2discord-operator/issues/45)) ([cc3f1c2](https://github.com/maverickd650/rss2discord-operator/commit/cc3f1c219152b62e6625ada3dc1dc7d64a2cdd77))

## [0.4.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.1...v0.4.2) (2026-06-21)


### Bug Fixes

* strip trailing "Continue reading" boilerplate, remove dead Embed.ImageURL ([#42](https://github.com/maverickd650/rss2discord-operator/issues/42)) ([04fc026](https://github.com/maverickd650/rss2discord-operator/commit/04fc0269b03fa8d284d5fcc15be8587578627ade))

## [0.4.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.0...v0.4.1) (2026-06-21)


### Bug Fixes

* fall back to catch-up when LastSeenEntry scrolls out of the feed window ([#38](https://github.com/maverickd650/rss2discord-operator/issues/38)) ([8af17de](https://github.com/maverickd650/rss2discord-operator/commit/8af17de3b212d54cf63b7c92adf3ef5306c58fec))
* handle non-UTF-8 feeds, link/title churn, relative Atom links, and HTML in titles ([#41](https://github.com/maverickd650/rss2discord-operator/issues/41)) ([d048fe9](https://github.com/maverickd650/rss2discord-operator/commit/d048fe96d2a281ea33ebb521ff41314cc1e6ff53))

## [0.4.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.3.0...v0.4.0) (2026-06-21)


### ⚠ BREAKING CHANGES

* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (2 → 3) ([#26](https://github.com/maverickd650/rss2discord-operator/issues/26))

### Features

* add fuzz test for feed parser, expose entry author/categories ([#34](https://github.com/maverickd650/rss2discord-operator/issues/34)) ([023ac5a](https://github.com/maverickd650/rss2discord-operator/commit/023ac5a5ad1b2281c9a310b3e18c915329610817))
* **chart:** add Grafana dashboard + PrometheusRule, shorten resource names ([#35](https://github.com/maverickd650/rss2discord-operator/issues/35)) ([f33fd1b](https://github.com/maverickd650/rss2discord-operator/commit/f33fd1b329ac9a33227396589933c20977be1aaa))
* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (2 → 3) ([#26](https://github.com/maverickd650/rss2discord-operator/issues/26)) ([492a51c](https://github.com/maverickd650/rss2discord-operator/commit/492a51cad1d7d36ddb17538fb75070e5f6bad714))


### Bug Fixes

* **chart:** add OCI source annotation so Renovate can find release notes ([#32](https://github.com/maverickd650/rss2discord-operator/issues/32)) ([b88fa0f](https://github.com/maverickd650/rss2discord-operator/commit/b88fa0f725c459c799ae4f3acf07c0923a733b02))
* **ci:** address zizmor findings in release workflow ([#30](https://github.com/maverickd650/rss2discord-operator/issues/30)) ([38f6f36](https://github.com/maverickd650/rss2discord-operator/commit/38f6f36279d219ea49795737dd3babc9872997cd))

## [0.3.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.2...v0.3.0) (2026-06-20)


### Features

* add per-outcome metrics and persistent-failure Events for feed processing ([#15](https://github.com/maverickd650/rss2discord-operator/issues/15)) ([dfbc96b](https://github.com/maverickd650/rss2discord-operator/commit/dfbc96b66fc143bfe125d148c8d001be4b1b59c3))
* skip re-fetching unchanged RSS feeds via conditional GET ([#13](https://github.com/maverickd650/rss2discord-operator/issues/13)) ([15bc767](https://github.com/maverickd650/rss2discord-operator/commit/15bc767398d28ce71ceb6731aaff58d8fbf155a7))
* support Discord embeds, forum channels, and webhook branding ([#12](https://github.com/maverickd650/rss2discord-operator/issues/12)) ([7f0438d](https://github.com/maverickd650/rss2discord-operator/commit/7f0438d1ed16f7afcbf0b50595afa5a409d1c499))


### Bug Fixes

* bound feed fan-out, tighten SSRF guard, sanitize embed URLs, stop infinite render retries ([#20](https://github.com/maverickd650/rss2discord-operator/issues/20)) ([0ebbd5b](https://github.com/maverickd650/rss2discord-operator/commit/0ebbd5b23275df06051f26f6566c62baa13dd809))
* cap initial backlog catch-up and strip HTML from Discord messages ([#10](https://github.com/maverickd650/rss2discord-operator/issues/10)) ([17bb2f5](https://github.com/maverickd650/rss2discord-operator/commit/17bb2f529a6e49c83ee49594b7a7d022904b783f))
* **chart:** restore Interval/RetryInterval validation in Helm CRD ([#22](https://github.com/maverickd650/rss2discord-operator/issues/22)) ([92934c1](https://github.com/maverickd650/rss2discord-operator/commit/92934c1e836f23c31806f6440aeffec52e67dcfa))
* prevent dropped entries from conditional GETs and prune stale feed status ([#14](https://github.com/maverickd650/rss2discord-operator/issues/14)) ([8e06824](https://github.com/maverickd650/rss2discord-operator/commit/8e06824cf03cf77e7f32265fd82192cd3fe76145))
* remove unused admission webhook scaffold from manager ([#16](https://github.com/maverickd650/rss2discord-operator/issues/16)) ([6c2317e](https://github.com/maverickd650/rss2discord-operator/commit/6c2317e7be39f034c63a329d96489bcec60bc8c0))
* skip no-op status writes and reject malformed Interval/RetryInterval at apply time ([#17](https://github.com/maverickd650/rss2discord-operator/issues/17)) ([4e06d44](https://github.com/maverickd650/rss2discord-operator/commit/4e06d44aedd11a283c640d6a8bd847a4ac4a6865))

## [0.2.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.1...v0.2.2) (2026-06-20)


### Bug Fixes

* disable informer cache for Secrets to match get-only RBAC ([#8](https://github.com/maverickd650/rss2discord-operator/issues/8)) ([b8a1271](https://github.com/maverickd650/rss2discord-operator/commit/b8a1271f6c9940e81fa5097555d6ff81c5da8900))

## [0.2.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.0...v0.2.1) (2026-06-20)


### Bug Fixes

* **chart:** point default image at ghcr.io drop CRDs by default on uninstall ([#6](https://github.com/maverickd650/rss2discord-operator/issues/6)) ([ada6b97](https://github.com/maverickd650/rss2discord-operator/commit/ada6b9788f72d5fe8cb8e67f43ea6a7faa243109))

## [0.2.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.1.0...v0.2.0) (2026-06-20)


### Features

* **init:** initial commit ([0a6f662](https://github.com/maverickd650/rss2discord-operator/commit/0a6f66260e153d241b5adc3563aa35623825da44))
* **perf:** performance and optimisation pass ([#4](https://github.com/maverickd650/rss2discord-operator/issues/4)) ([5d25cc7](https://github.com/maverickd650/rss2discord-operator/commit/5d25cc74c328276571c92271de62d89db1b3e5b7))


### Bug Fixes

* **ci:** efficiency ([#5](https://github.com/maverickd650/rss2discord-operator/issues/5)) ([ba6e7cb](https://github.com/maverickd650/rss2discord-operator/commit/ba6e7cb9c4aea889126a9105fdd775e9cf189a82))
