# Firefox BiDi Realm Isolation — Debug Log & Fix Roadmap

This document records the investigation into `Permission denied to access property "length"`
errors that prevent lorca bindings from working under Firefox, along with the current
state of each fix, known remaining issues, and the recommended next steps.

---

## Background

Firefox's WebDriver BiDi `script.addPreloadScript` runs each injected script in a
**sandbox realm** that is isolated from the page's normal JavaScript realm. This is
similar to how Firefox content scripts work in extensions.

The key security rule is **one-way**: page-realm code **cannot** read properties (e.g.
`.length`, `.prototype`, `.call`) of objects that belong to the sandbox realm. The
reverse is allowed — sandbox-realm code can freely access page-realm objects.

When the page-realm WebSocket fires its `onopen`/`onmessage` event, Firefox internally
accesses `.length` on the registered callback to determine the call arity. If that
callback is a sandbox-realm function, Firefox throws:

```
Permission denied to access property "length"
```

This kills the event handler silently — meaning `window.__lorcaOpen` never becomes
`true`, all binding calls pile up in `window.__lorcaQueue`, and the UI appears blank
because every startup async call hangs indefinitely.

---

## Fixes Applied (in order)

### Fix 1 — Remove `contexts` from `script.addPreloadScript`

**File:** `firefox.go` → `injectScript`

**Symptom:** App crashed after ~2 seconds. Firefox's BiDi implementation does not
support the optional `contexts` parameter added in a later spec revision.

**Fix:** Removed `"contexts": []string{f.context}` from the `addPreloadScript` call.

**Status:** ✅ Resolved. App no longer crashes immediately.

---

### Fix 2 — Protocol guard in bootstrap

**File:** `relay.go` → `bootstrapTemplate`

**Symptom:** Preload script ran on `chrome://` error pages (neterror pages), generating
spurious cross-realm errors before the real app page loaded.

**Fix:** Added a guard at the top of the bootstrap IIFE:

```javascript
var _proto = window.location && window.location.protocol
if (_proto && _proto !== 'http:' && _proto !== 'https:' && _proto !== 'data:') { return }
```

**Status:** ✅ Resolved. No errors on error/neterror pages.

---

### Fix 3 — Convert `onopen`, `onmessage`, `__lorcaRegister` to page-realm

**File:** `relay.go` → `bootstrapTemplate`

**Symptom:** Page-realm WebSocket could not call the `onopen`/`onmessage` handlers
because they were sandbox-realm closures. `window.__lorcaOpen` stayed `false` forever.
All binding calls queued up, promises never resolved, Vue app appeared blank/gray.

**Fix:** Replaced all `function() {...}` handler definitions with `new window.Function(...)`
calls so the resulting function objects live in the page realm. Also replaced the
sandbox-realm `_register` closure with a page-realm `window.__lorcaRegister = new window.Function('name', ...)`.

**Status:** ✅ Partially resolved. Page now renders (Vue mounts, UI skeleton is visible).
However, bindings still do not work — see remaining issue below.

---

## Remaining Issue — Per-Binding Preload Scripts Cross-Realm Call

### Symptom

After Fix 3, ~40 `Permission denied to access property "length"` errors remain, one
per registered binding, reported at `127.0.0.1:PORT:1:XX` where the column varies
with the length of each binding name.

There is also a persistent error at `:37:9` originating from the Vite bundle, whose
source is not yet identified.

### Root Cause Hypothesis

**Each call to `script.addPreloadScript` may create a separate sandbox realm.**

lorca registers `N+1` preload scripts for Firefox:
1. The bootstrap (sets up WebSocket, `window.__lorcaRegister`, etc.)
2. One per binding: `() => { window.__lorcaRegister('bindingName') }`

If Firefox assigns a distinct sandbox realm to each `addPreloadScript` call, then:
- Bootstrap runs in realm **S1** → sets `window.__lorcaRegister` (page-realm, via
  `new window.Function`)
- Each per-binding script runs in realm **S2**, **S3**, … **SN**
- `window.__lorcaRegister` is a page-realm function, but when **S2** tries to call it,
  Firefox checks `window.__lorcaRegister.length` through a cross-realm Xray wrapper →
  `Permission denied`

This is distinct from the original issue (where page → sandbox was blocked). Here the
access path is **sandbox → page function** through an Xray wrapper; Firefox may still
enforce property-access restrictions on that path.

### Needs Verification

Before implementing the fix, confirm via a minimal test whether:

1. Firefox creates a **shared** sandbox for all `addPreloadScript` calls that omit the
   `sandbox` parameter, or whether each call gets its own realm.
2. Whether adding `"sandbox": "lorca"` (an explicit shared name) to all
   `addPreloadScript` calls makes them share a realm and resolves the issue.
3. Whether `script.evaluate` (used by `f.eval`) runs in **page realm** or in the
   sandbox realm. If it runs in page realm, `f.eval("window.__lorcaRegister('name')")` 
   would call sandbox-realm `__lorcaRegister` from page realm, which would also fail.

---

## Recommended Fix

### Option A — Inline registration in each per-binding preload script (preferred)

Instead of calling `window.__lorcaRegister('name')` from each per-binding preload
script (which may be a cross-realm call), generate the full binding inline so the
script contains no calls to functions from another realm.

Concretely, change `injectScript` (or add a dedicated `injectBinding` method to the
`browserImpl` interface) so that for Firefox, each per-binding preload script becomes:

```javascript
() => {
  window['bindingName'] = new window.Function(
    'var args = Array.prototype.slice.call(arguments);' +
    'var seq = (window["bindingName"]._seq = (window["bindingName"]._seq || 0) + 1);' +
    'return new Promise(function(resolve, reject) {' +
    '  window.__lorcaPending.set("bindingName:" + seq, {resolve: resolve, reject: reject});' +
    '  window.__lorcaSend(JSON.stringify({name: "bindingName", seq: seq, args: args}));' +
    '});'
  );
  window['bindingName']._seq = 0;
}
```

The binding name is hardcoded into the string — no closure, no cross-realm call.
`window.__lorcaPending` and `window.__lorcaSend` are page-realm objects accessed via
`window.*`, which is always allowed from sandbox realm.

**Implementation path:**
- Add `injectBinding(name string) error` to the `browserImpl` interface in `browser.go`
- Chrome's implementation: call `injectScript(fmt.Sprintf("window.__lorcaRegister('%s')", name))`
  (unchanged behavior)
- Firefox's implementation: generate the inline preload script above and also call
  `f.eval(inlineScript)` for the current page (same as now but with inline code)
- Update `ui.go` `Bind` to call `u.browser.injectBinding(name)` instead of
  `u.browser.injectScript(fmt.Sprintf("window.__lorcaRegister('%s')", name))`

Also: remove `f.eval(js)` from `injectScript` for Firefox, or at minimum stop using
`injectScript` for bindings. The `eval` path on the current page matters for bindings
registered before the app page loads; replacing it with an inline eval that also avoids
calling `__lorcaRegister` is safer.

### Option B — Shared sandbox name on all `addPreloadScript` calls

Add `"sandbox": "lorca-relay"` to every `script.addPreloadScript` call. If Firefox
shares the realm for scripts with the same sandbox name, all preload scripts run in the
same realm and calling `window.__lorcaRegister` is a same-realm call.

This is simpler code but depends on unverified Firefox behavior. Test first.

### Option C — Remove per-binding preload scripts; rely entirely on relay WebSocket

Remove the `addPreloadScript` call from `injectScript` for bindings. All binding
registration is handled by the relay WebSocket: when the page loads and the bootstrap's
WebSocket connects, `handleClient` replays all registered bindings as "register"
messages, and the page-realm `onmessage` handler processes them.

The risk is **timing**: if Vue's app code calls a binding synchronously before the
WebSocket "register" messages have been processed, the call will fail. To mitigate,
Vue can defer binding calls until the relay connection is confirmed (e.g. by awaiting a
`__lorcaReady` promise that resolves in `onopen`).

This option requires a small protocol change on the EggLedger side as well.

---

## Current State of `relay.go` Bootstrap

```javascript
(function() {
  var _proto = window.location && window.location.protocol
  if (_proto && _proto !== 'http:' && _proto !== 'https:' && _proto !== 'data:') { return }
  window.__lorcaWS = new window.WebSocket('ws://127.0.0.1:__RELAY_PORT__')
  window.__lorcaPending = new window.Map()
  window.__lorcaQueue = new window.Array()
  window.__lorcaOpen = false
  window.__lorcaSend = new window.Function('msg',
    'if (window.__lorcaOpen) { window.__lorcaWS.send(msg) }' +
    'else { window.__lorcaQueue.push(msg) }')
  window.__lorcaWS.onopen = new window.Function(
    'window.__lorcaOpen = true;' +
    'for (var i = 0; i < window.__lorcaQueue.length; i++) { window.__lorcaWS.send(window.__lorcaQueue[i]) }' +
    'window.__lorcaQueue = new window.Array()')
  window.__lorcaRegister = new window.Function('name',
    'window[name] = function() {' +
    '  var args = Array.prototype.slice.call(arguments);' +
    '  var seq = (window[name]._seq = (window[name]._seq || 0) + 1);' +
    '  return new Promise(function(resolve, reject) {' +
    '    window.__lorcaPending.set(name + \':\' + seq, {resolve: resolve, reject: reject});' +
    '    window.__lorcaSend(JSON.stringify({name: name, seq: seq, args: args}));' +
    '  });' +
    '};' +
    'window[name]._seq = 0;')
  window.__lorcaWS.onmessage = new window.Function('e',
    'var msg = window.JSON.parse(e.data);' +
    'if (msg.type === \'register\') {' +
    '  window.__lorcaRegister(msg.name);' +
    '} else if (msg.type === \'result\') {' +
    '  var cb = window.__lorcaPending.get(msg.name + \':\' + msg.seq);' +
    '  if (cb) {' +
    '    if (msg.error) { cb.reject(new window.Error(msg.error)); } else { cb.resolve(msg.result); }' +
    '    window.__lorcaPending.delete(msg.name + \':\' + msg.seq);' +
    '  }' +
    '}')
})()
```

---

## Files to Change

| File | What to change |
|---|---|
| `relay.go` | `bootstrapTemplate` — already updated (Fix 3 above) |
| `browser.go` | Add `injectBinding(name string) error` to the `browserImpl` interface |
| `ui.go` | `Bind` — call `u.browser.injectBinding(name)` instead of `u.browser.injectScript(...)` |
| `chrome.go` | Implement `injectBinding` as a thin wrapper around `injectScript` (no change in behavior) |
| `firefox.go` | Implement `injectBinding` with inline page-realm function generation; remove `f.eval` cross-realm call for the current page |

---

## Open Questions

1. Does `script.evaluate` (used by `f.eval`) run in page realm or sandbox realm when
   called with `target: {context: id}` and no `sandbox` field? If sandbox realm, the
   eval path also needs fixing.

2. What is at line 37, column 9 of the compiled Vite bundle that triggers "Permission
   denied to access property 'length'"? This error is constant across all bootstrap
   changes, so it does not originate from lorca's injected scripts. It may be Vue's
   reactivity system or a bundled library inspecting `window` properties.

3. Is the `window.__lorcaRegister` path inside `onmessage` (which IS page-realm after
   Fix 3) actually working? If the relay's "register" messages arrive and are processed,
   bindings set up that way should work even if per-binding preload scripts fail. A
   quick test: add a delay before the first Vue binding call and check if relay
   messages have time to register bindings.
