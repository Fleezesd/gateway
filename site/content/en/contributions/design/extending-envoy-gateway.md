---
title: "Envoy Gateway Extensions Design"
---

As outlined in the [official goals][] for the Envoy Gateway project, one of the main goals is to "provide a common foundation for vendors to build value-added products
without having to re-engineer fundamental interactions". Development of the Envoy Gateway project has been focused on developing the core features for the project and
Kubernetes Gateway API conformance. This system focuses on the “common foundation for vendors” component by introducing a way for vendors to extend Envoy Gateway.

To meaningfully extend Envoy Gateway and provide additional features, Extensions need to be able to introduce their own custom resources and have a high level of control
over the configuration generated by Envoy Gateway. Simply applying some static xDS configuration patches or relying on the existing Gateway API resources are both insufficient on their own
as means to add larger features that require dynamic user-configuration.

As an example, an extension developer may wish to provide their own out-of-the-box authentication filters that require configuration from the end-user. This is a scenario where the ability to introduce
custom resources and attach them to [HTTPRoute][]s as an [ExtensionRef][] is necessary. Providing the same feature through a series of xDS patch resources would be too cumbersome for many end-users that want to avoid
that level of complexity when managing their clusters.  

## Goals

- Provide a foundation for extending the Envoy Gateway control plane
- Allow Extension Developers to introduce their own custom resources for extending the Gateway-API via [ExtensionRefs][], [policyAttachments][] (future) and [backendRefs][] (future).
- Extension developers should **NOT** have to maintain a custom fork of Envoy Gateway
- Provide a system for extending Envoy Gateway which allows extension projects to ship updates independent of Envoy Gateway's release schedule
- Modify the generated Envoy xDS config
- Setup a foundation for the initial iteration of Extending Envoy Gateway
- Allow an Extension to hook into the infra manager pipeline (future)

## Non-Goals

- The initial design does not capture every hook that Envoy Gateway will eventually support.
- Extend [Gateway API Policy Attachments][]. At some point, these will be addressed using this extension system, but the initial implementation omits these.
- Support multiple extensions at the same time. Due to the fact that extensions will be modifying xDS resources after they are generated, handling the order of extension execution for each individual hook point is a challenge. Additionally, there is no
real way to prevent one extension from overwriting or breaking modifications to xDS resources that were made by another extension that was executed first.

## Overview

Envoy Gateway can be extended by vendors by means of an extension server developed by the vendor and deployed alongside Envoy Gateway.
An extension server can make use of one or more pre/post hooks inside Envoy Gateway before and after its major components (translator, etc.) to allow the extension to modify the data going into or coming out of these components.
An extension can be created external to Envoy Gateway as its own Kubernetes deployment or loaded as a sidecar. gRPC is used for the calls between Envoy Gateway and an extension. In the hook call, Envoy Gateway sends data as well
as context information to the extension and expects a reply with a modified version of the data that was sent to the extension. Since extensions fundamentally alter the logic and data that Envoy Gateway provides, Extension projects assume responsibility for any bugs and issues
they create as a direct result of their modification of Envoy Gateway.

## Diagram

![Architecture](/img/extension-example.png)

## Registering Extensions in Envoy Gateway

Information about the extension that Envoy Gateway needs to load is configured in the Envoy Gateway config.

An example configuration:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyGateway
extensionManager:
  policyResources:
  - group: example.myextension.io
    version: v1alpha1
    kind: ListenerPolicyKind
  resources:
  - group: example.myextension.io
    version: v2
    kind: OAuth2Filter
  hooks:
    xdsTranslator:
      post:
      - Route
      - VirtualHost
      - HTTPListener
      - Translation
  service:
    fqdn:
      hostname: my-extension.example
      port: 443
    tls:
      certificateRef:
        name: my-secret
        namespace: default
```

An extension must supply connection information in the `extension.service` field so that Envoy Gateway can communicate with the extension. The `tls` configuration is optional. Envoy Gateway supports connecting to an extension server either with TCP or with Unix Domain Sockets as a transport layer.

If the extension wants Envoy Gateway to watch resources for it then the extension must configure the optional `extension.resources` field and supply a list of:

- `group`: the API group of the resource
- `version`: the API version of the resource
- `kind`: the Kind of resource

If the extension wants Envoy Gateway to watch for policy resources then it must configure the optional `extensions.policyResources` field and supply a list of

- `group`: the API group of the resource
- `version`: the API version of the resource
- `kind`: the Kind of resource

Policy resources, like all Gateway-API policies, must contain `targetRef` or `targetRefs` fields in the spec which allow Envoy Gateway to identify which resources are targeted by the policy. 
Policies can currently only target `Gateway` resources, and are provided as context to calls to the `HTTPListener` hook.

The extension can configure the `extensionManager.hooks` field to specify which hook points it would like to support. If a given hook is not listed here then it will not be executed even
if the extension is configured properly. This allows extension developers to only opt-in to the hook points they want to make use of.

This configuration is required to be provided at bootstrap and modifying the registered extension during runtime is not currently supported.
Envoy Gateway will keep track of the registered extension and its API `groups` and `kinds` when processing Gateway API resources.

## Extending Gateway API and the Data Plane

Envoy Gateway manages [Envoy][] deployments, which act as the data plane that handles actual user traffic. Users configure the data plane using the K8s Gateway API resources which Envoy
Gateway converts into [Envoy specific configuration (xDS)][] to send over to Envoy.

Gateway API offers [ExtensionRef filters][] and [Policy Attachments][] as extension points for implementers to use. Envoy Gateway extends the Gateway API using these extension points to provide support for [rate limiting][]
and [authentication][] native to the project. The initial design of Envoy Gateway extensions will primarily focus on `ExtensionRef` filters so that extension developers can reference their own resources as HTTP Filters in the same way
that Envoy Gateway has native support for rate limiting and authentication filters, as well as policy resources which can target `Gateway`s.

When Envoy Gateway encounters an [HTTPRoute][] or [GRPCRoute][] that has an `ExtensionRef` `filter` with a `group` and `kind` that Envoy Gateway does not support, it will first
check the registered extension to determine if it supports the referenced object before considering it a configuration error.

This allows users to be able to reference additional filters provided by their Envoy Gateway Extension, in their `HTTPRoute`s / `GRPCRoute`s:

```yaml
apiVersion: example.myextension.io/v1alpha1
kind: OAuth2Filter
metadata:
  name: oauth2-filter
spec:
  ...

---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example
spec:
  parentRefs:
  - name: eg
  hostnames:
  - www.example.com
  rules:
  - clientSelectors:
    - path:
        type: PathPrefix
        value: /
    filters:
    - type: ExtensionRef
      extensionRef:
        group: example.myextension.io
        kind: OAuth2Filter
        name: oauth2-filter
    backendRefs:
    - name: backend
      port: 3000
```

In order to enable the usage of new resources introduced by an extension for translation and xDS modification, Envoy Gateway provides hook points within the translation pipeline, where it calls out to the extension service registered in the [EnvoyGateway config][]
if they specify an `group` that matches the `group` of an `ExtensionRef` filter. The extension will then be able to modify the xDS that Envoy Gateway generated and send back the
modified configuration. If an extension is not registered or if the registered extension does not specify support for the `group` of an `ExtensionRef` filter then Envoy Gateway will treat it as an unknown resource
and provide an error to the user.

**Note:** Currently (as of [v1][]) Gateway API does not provide a means to specify the namespace or version of an object referenced as an `ExtensionRef`. The extension mechanism will assume that
the namespace of any `ExtensionRef` is the same as the namespace of the `HTTPRoute` or `GRPCRoute` it is attached to rather than treating the `name` field of an `ExtensionRef` as a `name.namespace` string.
If Gateway API adds support for these fields then the design of the Envoy Gateway extensions will be updated to support them.

Similarly, any registered policy resource that targets an `HTTPListener` will be sent to the `HTTPListener` hook as context.

## Watching New Resources

Envoy Gateway will dynamically create new watches on resources introduced by the registered Extension. It does so by using the [controller-runtime][] to create new watches on [Unstructured][] resources that match the `version`s, `group`s, and `kind`s that the registered extension configured. When communicating with an extension, Envoy Gateway sends these Unstructured resources over to the extension. This eliminates the need for the extension to create its own watches which would have a strong chance of creating race conditions and reconciliation loops when resources change. When an extension receives the Unstructured resources from Envoy Gateway it can perform its own type validation on them. Currently we make the simplifying assumption that the registered extension's `Kinds` are filters referenced by `extensionRef` in `HTTPRouteFilter`s . Policy attachments which target `Gateway` resources work in the same way. 

## xDS Hooks API

Envoy Gateway supports the following hooks as the initial foundation of the Extension system. Additional hooks can be developed using this extension system at a later point as new use-cases and needs are discovered. The primary iteration of the extension hooks
focuses solely on the modification of xDS resources.

### Route Modification Hook

The [Route][] level Hook provides a way for extensions to modify a route generated by Envoy Gateway before it is finalized.
Doing so allows extensions to configure/modify route fields configured by Envoy Gateway and also to configure the
Route's TypedPerFilterConfig which may be desirable to do things such as pass settings and information to ext_authz filters.
The Post Route Modify hook also passes a list of Unstructured data for the externalRefs owned by the extension on the HTTPRoute that created this xDS route
This hook is always executed when an extension is loaded that has added `Route` to the `EnvoyProxy.extensionManager.hooks.xdsTranslator.post`, and only on Routes which were generated from an HTTPRoute that uses extension resources as externalRef filters.

```go
// PostRouteModifyRequest sends a Route that was generated by Envoy Gateway along with context information to an extension so that the Route can be modified
message PostRouteModifyRequest {
    envoy.config.route.v3.Route route = 1;
    PostRouteExtensionContext post_route_context = 2;
}

// RouteExtensionContext provides resources introduced by an extension and watched by Envoy Gateway
// additional context information can be added to this message as more use-cases are discovered
message PostRouteExtensionContext {
    // Resources introduced by the extension that were used as extensionRefs in an HTTPRoute/GRPCRoute
    repeated ExtensionResource extension_resources = 1;

    // hostnames are the fully qualified domain names attached to the HTTPRoute
    repeated string hostnames = 2;
}

// ExtensionResource stores the data for a K8s API object referenced in an HTTPRouteFilter
// extensionRef. It is constructed from an unstructured.Unstructured marshalled to JSON. An extension
// can marshal the bytes from this resource back into an unstructured.Unstructured and then 
// perform type checking to obtain the resource.
message ExtensionResource {
    bytes unstructured_bytes = 1;
}

// PostRouteModifyResponse is the expected response from an extension and contains a modified version of the Route that was sent
// If an extension returns a nil Route then it will not be modified
message PostRouteModifyResponse {
    envoy.config.route.v3.Route route = 1;
}
```

### VirtualHost Modification Hook

The [VirtualHost][] Hook provides a way for extensions to modify a VirtualHost generated by Envoy Gateway before it is finalized.
An extension can also make use of this hook to generate and insert entirely new Routes not generated by Envoy Gateway.
This hook is always executed when an extension is loaded that has added `VirtualHost` to the `EnvoyProxy.extensionManager.hooks.xdsTranslator.post`.
An extension may return nil to not make any changes to the VirtualHost.

```protobuf
// PostVirtualHostModifyRequest sends a VirtualHost that was generated by Envoy Gateway along with context information to an extension so that the VirtualHost can be modified
message PostVirtualHostModifyRequest {
    envoy.config.route.v3.VirtualHost virtual_host = 1;
    PostVirtualHostExtensionContext post_virtual_host_context = 2;
}

// Empty for now but we can add fields to the context as use-cases are discovered without
// breaking any clients that use the API
// additional context information can be added to this message as more use-cases are discovered
message PostVirtualHostExtensionContext {}

// PostVirtualHostModifyResponse is the expected response from an extension and contains a modified version of the VirtualHost that was sent
// If an extension returns a nil Virtual Host then it will not be modified
message PostVirtualHostModifyResponse {
    envoy.config.route.v3.VirtualHost virtual_host = 1;
}
```

### HTTP Listener Modification Hook

The HTTP [Listener][] modification hook is the broadest xDS modification Hook available and allows an extension to make changes to a Listener generated by Envoy Gateway before it is finalized.
This hook is always executed when an extension is loaded that has added `HTTPListener` to the `EnvoyProxy.extensionManager.hooks.xdsTranslator.post`. An extension may return nil
in order to not make any changes to the Listener.

```protobuf
// PostVirtualHostModifyRequest sends a Listener that was generated by Envoy Gateway along with context information to an extension so that the Listener can be modified
message PostHTTPListenerModifyRequest {
    envoy.config.listener.v3.Listener listener = 1;
    PostHTTPListenerExtensionContext post_listener_context = 2;
}

// Empty for now but we can add fields to the context as use-cases are discovered without
// breaking any clients that use the API
// additional context information can be added to this message as more use-cases are discovered
message PostHTTPListenerExtensionContext {
    // Resources introduced by the extension that were used as extension server
    // policies targeting the listener
    repeated ExtensionResource extension_resources = 1;
}

// PostHTTPListenerModifyResponse is the expected response from an extension and contains a modified version of the Listener that was sent
// If an extension returns a nil Listener then it will not be modified
message PostHTTPListenerModifyResponse {
    envoy.config.listener.v3.Listener listener = 1;
}
```

### Post xDS Translation Modify Hook

The Post Translate Modify hook allows an extension to modify the clusters and secrets in the xDS config.
This allows for inserting clusters that may change along with extension specific configuration to be dynamically created rather than
using custom bootstrap config which would be sufficient for clusters that are static and not prone to have their configurations changed.
An example of how this may be used is to inject a cluster that will be used by an ext_authz http filter created by the extension.
The list of clusters and secrets returned by the extension are used as the final list of all clusters and secrets
This hook is always executed when an extension is loaded that has added `Translation` to the `EnvoyProxy.extensionManager.hooks.xdsTranslator.post`.

```protobuf
// PostTranslateModifyRequest currently sends only clusters and secrets to an extension.
// The extension is free to add/modify/remove the resources it received.
message PostTranslateModifyRequest {
    PostTranslateExtensionContext post_translate_context = 1;
    repeated envoy.config.cluster.v3.Cluster clusters = 2;
    repeated envoy.extensions.transport_sockets.tls.v3.Secret secrets = 3;
}

// PostTranslateModifyResponse is the expected response from an extension and contains
// the full list of xDS clusters and secrets to be used for the xDS config.
message PostTranslateModifyResponse {
    repeated envoy.config.cluster.v3.Cluster clusters = 1;
    repeated envoy.extensions.transport_sockets.tls.v3.Secret secrets = 2;
}
```

### Extension Service

Currently, an extension must implement all of the following hooks although it may return the input(s) it received
if no modification of the resource is desired. A future expansion of the extension hooks will allow an Extension to specify
with config which Hooks it would like to "subscribe" to and which Hooks it does not wish to support. These specific Hooks were chosen
in order to provide extensions with the ability to have both broad and specific control over xDS resources and to minimize the amount of data being sent.

```protobuf
service EnvoyGatewayExtension {
    rpc PostRouteModify (PostRouteModifyRequest) returns (PostRouteModifyResponse) {};
    rpc PostVirtualHostModify(PostVirtualHostModifyRequest) returns (PostVirtualHostModifyResponse) {};
    rpc PostHTTPListenerModify(PostHTTPListenerModifyRequest) returns (PostHTTPListenerModifyResponse) {};
    rpc PostTranslateModify(PostTranslateModifyRequest) returns (PostTranslateModifyResponse) {};
}
```

## Design Decisions

- Envoy Gateway watches new custom resources introduced by a loaded extension and passes the resources back to the extension when they are used.
  - This decision was made to solve the problem about how resources introduced by an extension get watched. If an extension server watches its own resources then it would need some way to trigger an Envoy Gateway reconfigure when a resource that Envoy Gateway is not watching gets updated. Having Envoy Gateway watch all resources removes any concern about creating race conditions or reconcile loops that would result from Envoy Gateway and the extension server both having so much separate state that needs to be synchronized.
- The Extension Server takes ownership of producing the correct xDS configuration in the hook responses
- The Extension Server will be responsible for ensuring the performance of the hook processing time
- The Post xDS level gRPC hooks all currently send a context field even though it contains nothing for several hooks. These fields exist so that they can be updated in the future to pass
additional information to extensions as new use-cases and needs are discovered.
- The initial design supplies the scaffolding for both "pre xDS" and "post xDS" hooks. Only the post hooks are currently implemented which operate on xDS resources after they have been generated.
The pre hooks will be implemented at a later date along with one or more hooks in the infra manager. The infra manager level hook(s) will exist to power use-cases such as dynamically creating Deployments/Services for the extension the
whenever Envoy Gateway creates an instance of Envoy Proxy. An extension developer might want to take advantage of this functionality to inject a new authorization service as a sidecar on the Envoy Proxy deployment for reduced latency.
- Multiple extensions are not be supported at the same time. Preventing conflict between multiple extensions that are mangling xDS resources is too difficult to ensure compatibility with and is likely to only generate issues.

## Known Challenges

Extending Envoy Gateway by using an external extension server which makes use of hook points in Envoy Gateway does comes with a few trade-offs. One known trade-off is the impact of the time that it takes for the hook calls to be executed. Since an extension would make use of hook points in Envoy Gateway that use gRPC for communication, the time it takes to perform these requests could become a concern for some extension developers. One way to minimize the request time of the hook calls is to load the extension server as a sidecar to Envoy Gateway using the Unix Local Domain transport to minimize the impact of networking on the hook calls.

[official goals]: https://github.com/envoyproxy/gateway/blob/main/GOALS.md#extensibility
[ExtensionRef filters]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1.LocalObjectReference
[ExtensionRef]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1.LocalObjectReference
[ExtensionRefs]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1.LocalObjectReference
[backendRefs]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1.BackendObjectReference
[Gateway API Policy attachments]: https://gateway-api.sigs.k8s.io/references/policy-attachment/?h=policy
[Policy Attachments]: https://gateway-api.sigs.k8s.io/references/policy-attachment/?h=policy
[policyAttachments]: https://gateway-api.sigs.k8s.io/references/policy-attachment/?h=policy
[Envoy]: https://www.envoyproxy.io/
[Envoy specific configuration (xDS)]: https://www.envoyproxy.io/docs/envoy/v1.25.1/configuration/configuration
[v1]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1
[rate limiting]: ./rate-limit
[authentication]: ../../latest/tasks/security/jwt-authentication
[HTTPRoute]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1.HTTPRoute
[GRPCRoute]: https://gateway-api.sigs.k8s.io/reference/spec/#gateway.networking.k8s.io/v1alpha2.GRPCRoute
[EnvoyGateway config]: ../../latest/api/extension_types#envoygateway
[controller-runtime]: https://github.com/kubernetes-sigs/controller-runtime
[Unstructured]: https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1/unstructured
[Listener]: https://www.envoyproxy.io/docs/envoy/v1.23.0/api-v3/config/listener/v3/listener.proto#config-listener-v3-listener
[VirtualHost]: https://www.envoyproxy.io/docs/envoy/v1.23.0/api-v3/config/route/v3/route_components.proto#config-route-v3-virtualhost
[Route]: https://www.envoyproxy.io/docs/envoy/v1.23.0/api-v3/config/route/v3/route_components.proto#config-route-v3-route
