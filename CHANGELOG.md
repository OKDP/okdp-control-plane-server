# Changelog

## [0.7.0](https://github.com/OKDP/okdp-control-plane-server/compare/okdp-server-0.6.0...okdp-server-0.7.0) (2026-07-17)


### Features

* add a Helm chart for the server ([#19](https://github.com/OKDP/okdp-control-plane-server/issues/19)) ([3a7b63e](https://github.com/OKDP/okdp-control-plane-server/commit/3a7b63e18f42f3f73269c72302f7bd7a8d800686))
* build and publish the server image (Dockerfile + CI) ([f86a49d](https://github.com/OKDP/okdp-control-plane-server/commit/f86a49dd2c5c244e89a27473b5c3f8ef3ee29ce0))
* kubocd context cloning per project ([17222ba](https://github.com/OKDP/okdp-control-plane-server/commit/17222bac3cc3842171649ca61330234247e40f43))
* manage the service catalog from the Control Plane ([#13](https://github.com/OKDP/okdp-control-plane-server/issues/13)) ([db7a914](https://github.com/OKDP/okdp-control-plane-server/commit/db7a914fed861ab4b7ec31406ef6e01aba32db67))
* only expose a service URL when an Ingress routes to it ([06eb656](https://github.com/OKDP/okdp-control-plane-server/commit/06eb656b33f106762701bcc81c1250ea121ccd2b))
* per service package repository override ([#22](https://github.com/OKDP/okdp-control-plane-server/issues/22)) ([11c87ad](https://github.com/OKDP/okdp-control-plane-server/commit/11c87ad50b2ab6d41e1442e5ff2a882c0d3c147f))
* project handler backed by k8s namespaces ([f61b9fa](https://github.com/OKDP/okdp-control-plane-server/commit/f61b9fac6ed8dc67ec053f348f751fd359d4b1c5))
* secrets and keycloak integration ([8696aad](https://github.com/OKDP/okdp-control-plane-server/commit/8696aadc4b4dbe3efadcfa982c6e951747dae77a))
* spark applications endpoints ([5b603a7](https://github.com/OKDP/okdp-control-plane-server/commit/5b603a7e0939133f3502e5b77a6f14235cde96dc))
* spark history server endpoints ([98019df](https://github.com/OKDP/okdp-control-plane-server/commit/98019df81ef50b7b3c534d8e19f4172f6290a63b))
* support updating a project description (PUT /api/projects/:name) ([e87de5e](https://github.com/OKDP/okdp-control-plane-server/commit/e87de5e5490e76891b894d76e143ba4b2c59f845))


### Bug Fixes

* grant the server update/patch on namespaces for project edits ([90a34f7](https://github.com/OKDP/okdp-control-plane-server/commit/90a34f70ccc1ba26f287171602977f129cccee66))
