# Changelog

## [0.7.1](https://github.com/OKDP/okdp-control-plane-server/compare/v0.7.0...v0.7.1) (2026-08-23)


### Bug Fixes

* **catalog:** nest the services under their console section in the Context ([41ddf2e](https://github.com/OKDP/okdp-control-plane-server/commit/41ddf2eb8b665af5399959645b54c921259b69d4))

## [0.7.0](https://github.com/OKDP/okdp-control-plane-server/compare/v0.6.0...v0.7.0) (2026-08-21)


### ⚠ BREAKING CHANGES

* **context:** read the platform configuration from a single platform Context
* rename the Go module and chart to okdp-control-plane-server

### Features

* add a Helm chart for the server ([#19](https://github.com/OKDP/okdp-control-plane-server/issues/19)) ([3a7b63e](https://github.com/OKDP/okdp-control-plane-server/commit/3a7b63e18f42f3f73269c72302f7bd7a8d800686))
* add menu metadata to the catalog API ([b2871e5](https://github.com/OKDP/okdp-control-plane-server/commit/b2871e535fb567e0a46a137f2a7dfbbe4b7704b3)), closes [#35](https://github.com/OKDP/okdp-control-plane-server/issues/35)
* build and publish the server image (Dockerfile + CI) ([f86a49d](https://github.com/OKDP/okdp-control-plane-server/commit/f86a49dd2c5c244e89a27473b5c3f8ef3ee29ce0))
* **capabilities:** publish the console OIDC client from the Context ([0588d75](https://github.com/OKDP/okdp-control-plane-server/commit/0588d755e45850795d09312633d556953d064ed8))
* **chart:** render the proxy variables ([19ccbb4](https://github.com/OKDP/okdp-control-plane-server/commit/19ccbb4dc1a53afe7b75ff22a8c3997d60f0f337))
* **connections:** manage external project connections ([743cdb3](https://github.com/OKDP/okdp-control-plane-server/commit/743cdb354ad0e133ff396a7be2e7b7bd98b9e91b))
* **context:** read the platform configuration from a single platform Context ([9e9ecfd](https://github.com/OKDP/okdp-control-plane-server/commit/9e9ecfd38b956e7c41dadf50af24cac988a4030e))
* **identity:** answer a structured 501 when the platform does not provision OIDC clients ([e57ba38](https://github.com/OKDP/okdp-control-plane-server/commit/e57ba38a2937fe32eba004be3e92a2103fca492f))
* **identity:** gate the identity API and add a capabilities endpoint ([b8040ef](https://github.com/OKDP/okdp-control-plane-server/commit/b8040efec652d5e1b2701115021a9f91857edb60))
* **identity:** gate the identity API and publish a capabilities endpoint ([b79b72f](https://github.com/OKDP/okdp-control-plane-server/commit/b79b72fb23069b2ed9363cbade376627735776f8))
* kubocd context cloning per project ([17222ba](https://github.com/OKDP/okdp-control-plane-server/commit/17222bac3cc3842171649ca61330234247e40f43))
* manage the service catalog from the Control Plane ([#13](https://github.com/OKDP/okdp-control-plane-server/issues/13)) ([db7a914](https://github.com/OKDP/okdp-control-plane-server/commit/db7a914fed861ab4b7ec31406ef6e01aba32db67))
* only expose a service URL when an Ingress routes to it ([06eb656](https://github.com/OKDP/okdp-control-plane-server/commit/06eb656b33f106762701bcc81c1250ea121ccd2b))
* per service package repository override ([#22](https://github.com/OKDP/okdp-control-plane-server/issues/22)) ([11c87ad](https://github.com/OKDP/okdp-control-plane-server/commit/11c87ad50b2ab6d41e1442e5ff2a882c0d3c147f))
* project handler backed by k8s namespaces ([f61b9fa](https://github.com/OKDP/okdp-control-plane-server/commit/f61b9fac6ed8dc67ec053f348f751fd359d4b1c5))
* **schema:** support quoted values in title options ([459023d](https://github.com/OKDP/okdp-control-plane-server/commit/459023d174b5146928ac60e9bab5aae1029b9c7f))
* secrets and keycloak integration ([8696aad](https://github.com/OKDP/okdp-control-plane-server/commit/8696aadc4b4dbe3efadcfa982c6e951747dae77a))
* **service:** fall back to the &lt;service&gt;[-console]-&lt;namespace&gt; host convention ([1bdb5d1](https://github.com/OKDP/okdp-control-plane-server/commit/1bdb5d1fde5e88df79f6f9379f7de821962eb1a8))
* **service:** resolve instance URL from role-aware host conventions ([931d84a](https://github.com/OKDP/okdp-control-plane-server/commit/931d84a6296d288a171778f851c272aaf9aae81e))
* spark applications endpoints ([5b603a7](https://github.com/OKDP/okdp-control-plane-server/commit/5b603a7e0939133f3502e5b77a6f14235cde96dc))
* spark history server endpoints ([98019df](https://github.com/OKDP/okdp-control-plane-server/commit/98019df81ef50b7b3c534d8e19f4172f6290a63b))
* support updating a project description (PUT /api/projects/:name) ([e87de5e](https://github.com/OKDP/okdp-control-plane-server/commit/e87de5e5490e76891b894d76e143ba4b2c59f845))
* typed project connections on the KuboCD Connection/Contract model ([eba4da2](https://github.com/OKDP/okdp-control-plane-server/commit/eba4da2557ff3b02e7082f559e504eac6b2a1f78))


### Bug Fixes

* **api:** let concurrent callers wait for the CRD probe ([08572ca](https://github.com/OKDP/okdp-control-plane-server/commit/08572ca9ae771d9ee4a2a3406d48b19a02726b59))
* **api:** resolve and validate the project on every project route ([d634b49](https://github.com/OKDP/okdp-control-plane-server/commit/d634b49515cd0da3b0276468afd8692ad2ab7994))
* **catalog:** report a package repository with no path instead of panicking ([b656037](https://github.com/OKDP/okdp-control-plane-server/commit/b6560379f240f5a9ba3d3973ca89375981251987))
* **catalog:** support registries requiring the anonymous token flow like ghcr.io ([34782cf](https://github.com/OKDP/okdp-control-plane-server/commit/34782cf6bdbf137780a54971e552796352b62f41))
* **chart:** default the allowed origin to an example host ([e719d60](https://github.com/OKDP/okdp-control-plane-server/commit/e719d60ed2627e8fa256dee7dce19b4a1e3714cc))
* **chart:** point the default image and the docs at the renamed repository ([2a6e621](https://github.com/OKDP/okdp-control-plane-server/commit/2a6e62163ceb9af3ace850f70921b9dd50f92722))
* **connections:** drop the replaced Secret only after the update lands ([ec8c114](https://github.com/OKDP/okdp-control-plane-server/commit/ec8c1144236e28306583dd3843189346020e2387))
* **connections:** keep the secret when the name was taken concurrently ([191de66](https://github.com/OKDP/okdp-control-plane-server/commit/191de66298f2fe04d99d5def15fe492280085b47))
* **connections:** refuse a secret another connection owns ([ee6fc99](https://github.com/OKDP/okdp-control-plane-server/commit/ee6fc99611e6a38cbb0a62e76daba4de929670ed))
* **connections:** refuse to adopt a credentials Secret we do not own ([aac28ce](https://github.com/OKDP/okdp-control-plane-server/commit/aac28ce92466b8f8dedd4b9bb44898eb48531f6b))
* grant the server update/patch on namespaces for project edits ([90a34f7](https://github.com/OKDP/okdp-control-plane-server/commit/90a34f70ccc1ba26f287171602977f129cccee66))
* **identity:** advertise user management only when its CRDs are served ([796662a](https://github.com/OKDP/okdp-control-plane-server/commit/796662a7b1c2b1365b16c928c00067eaa4d439e0))
* **identity:** keep the egress proxy when Keycloak TLS verification is off ([5c67cff](https://github.com/OKDP/okdp-control-plane-server/commit/5c67cff0d508f3a593d7fbb34b6fff809dcf72ce))
* **identity:** require every kubauth CRD before serving the identity API ([0ba1fe1](https://github.com/OKDP/okdp-control-plane-server/commit/0ba1fe1507c0d473be976ab8d02588b18a387cb7))
* **identity:** resolve the OIDC client provisioning from one field ([ccd9992](https://github.com/OKDP/okdp-control-plane-server/commit/ccd9992dc79c09b6426d269e4a412bcebd4313bb))
* **identity:** validate every provisioning provider at startup ([d99b35b](https://github.com/OKDP/okdp-control-plane-server/commit/d99b35b44bb425710a340e8956c8cf26fad8cf8a))
* **logs:** stream long log lines and report scanner failures ([f299903](https://github.com/OKDP/okdp-control-plane-server/commit/f299903ce35b6476ddfe8261e3730a7d45f7d1e1))
* **rbac:** grant the resources the server actually reads ([6c35df2](https://github.com/OKDP/okdp-control-plane-server/commit/6c35df2f4c6ac4cc450f12faa3178e1aa5d7ab2a))
* **secrets:** guard each external-secrets group on the CRD it uses ([942a154](https://github.com/OKDP/okdp-control-plane-server/commit/942a1547bd151b69f9b4e7baef15c24708659511))
* **server:** bound outgoing calls and incoming connections with timeouts ([ee56295](https://github.com/OKDP/okdp-control-plane-server/commit/ee562950c1f7adc7d46857315ef1ebac1a1b2791))
* **services:** expose the Spark UIs through the web proxy host ([34b6600](https://github.com/OKDP/okdp-control-plane-server/commit/34b66004bd61108221be890860c7f8573d46d8ea))
* **services:** resolve SSE service URLs through the ingress ([831b383](https://github.com/OKDP/okdp-control-plane-server/commit/831b38328c2fd2d568fab2fa062cec6be6a77446))
* **services:** scope service deletion to the exact release ([8357454](https://github.com/OKDP/okdp-control-plane-server/commit/8357454b96e9d7c185eb1a626af3da677384b188))


### Performance Improvements

* **api:** probe the CRDs outside the availability lock ([d1b7bc7](https://github.com/OKDP/okdp-control-plane-server/commit/d1b7bc78fc3a7cffc5e3eadb54f6922231f0f108))
* **catalog:** cache registry tag listings for five minutes ([8b52033](https://github.com/OKDP/okdp-control-plane-server/commit/8b52033e8e7fa2643e7a9771e91004fda2f63569))
* **catalog:** fall back to the default version when a tag listing fails ([9e2a3d0](https://github.com/OKDP/okdp-control-plane-server/commit/9e2a3d0e5b6f078ade0ee8a8ea7621ea2978cda3))
* **catalog:** resolve the package repository once and list OCI tags in parallel ([02959d1](https://github.com/OKDP/okdp-control-plane-server/commit/02959d17a4f0d7a1abc687dd5547793fa5a7c7e0))
* **context:** cache the platform Context for a short TTL ([0f82abc](https://github.com/OKDP/okdp-control-plane-server/commit/0f82abcec95c6a905a6be9327d9e7f415792f892))
* **context:** invalidate the cache after every catalog write ([ddb9f1e](https://github.com/OKDP/okdp-control-plane-server/commit/ddb9f1e8162d5be6d14ae343e47b904d476c95fb))
* **k8s:** raise the client rate limit above the client-go defaults ([6ccd30e](https://github.com/OKDP/okdp-control-plane-server/commit/6ccd30ec3b7f0659227e664b15987da9e3c954f6))
* **metrics:** add a per-project metrics endpoint ([a26efa4](https://github.com/OKDP/okdp-control-plane-server/commit/a26efa4c68c6ced3dcc59050bcdade1f2067c1e0))
* **metrics:** aggregate project metrics from two namespace lists ([90318bc](https://github.com/OKDP/okdp-control-plane-server/commit/90318bc4ffe836e8128adcd8425f8bb5df6a60cb))
* **metrics:** correct the stale per-pod comment ([99576ea](https://github.com/OKDP/okdp-control-plane-server/commit/99576eadd8a115a972c1002e8889ae373e6f53d7))
* **metrics:** give the project metrics route its own swagger block ([53dfa88](https://github.com/OKDP/okdp-control-plane-server/commit/53dfa884251a8d0509718065e5b729288d291774))
* **metrics:** list pod metrics once per namespace instead of per pod ([4fa7efb](https://github.com/OKDP/okdp-control-plane-server/commit/4fa7efbf00a71eb8503b8758e798fd33fca92364))
* **schema:** collapse concurrent cache misses with singleflight ([57b6b34](https://github.com/OKDP/okdp-control-plane-server/commit/57b6b3416b460fb778ad5f1cab28a527dc5e554c))
* **services:** list pods once per namespace instead of once per instance ([4ef3cbf](https://github.com/OKDP/okdp-control-plane-server/commit/4ef3cbf4de2482f24faf17161b8ee7faf251b684))


### Code Refactoring

* rename the Go module and chart to okdp-control-plane-server ([6849239](https://github.com/OKDP/okdp-control-plane-server/commit/6849239f28154edadcf83ed33d559780835c3821))
