-- Argo CD health check for every kflare kind (kflare.dev/*), from kflare's Ready condition.
--
-- kflare reports each resource's state in one condition, Ready, set for the generation it last reconciled
-- (its observedGeneration; kflare sets no status.observedGeneration). docs/deploy.md, "Health checks in
-- Argo CD and Flux", explains it:
--   Healthy      Ready is True for the current generation
--   Degraded     Ready is False for the current generation, for a reason a person must act on
--   Suspended    the kflare.dev/paused annotation stops kflare from reconciling the resource
--   Progressing  anything else: kflare has not reconciled the current spec yet, or waits for
--                Cloudflare, for a rate limit, or for another resource
--
-- Argo CD opens only Lua's base, table and package libraries for a script in argocd-cm, so this one uses
-- no string function. config/argocd/health_test.yaml holds its tests, in Argo CD's format.

-- The reasons of Ready=False that wait for a person rather than for time: Cloudflare refused the spec,
-- another resource holds the object, the object exists or is missing against the adoption policy, or a
-- setting is wrong. Every other reason clears up by itself: an API error or a rate limit is retried, and
-- a missing or unready parent is waited for.
local needsPerson = {
  -- Cloudflare refused the request (4xx), or kflare cannot build a client from the token
  TerminalError = true,
  InvalidToken = true,
  -- The CloudflareAccount, or a reference, does not fit
  AccountNotAllowed = true,
  AccountMismatch = true,
  InvalidAllowedNamespaces = true,
  R2BucketJurisdictionUnsupported = true,
  -- A Secret or ConfigMap exists but lacks the key the spec names
  TokenKeyMissing = true,
  ScriptKeyMissing = true,
  BindingSecretKeyMissing = true,
  -- Adoption (the kflare.dev/adoption-policy annotation)
  AlreadyExists = true,
  NotFound = true,
  InvalidAdoptionPolicy = true,
  -- Another resource manages the Cloudflare object, or configures the same settings
  NameConflict = true,
  TitleConflict = true,
  PatternConflict = true,
  RulesetConflict = true,
  CredentialsSecretConflict = true,
  AlreadyConfigured = true,
  TunnelAlreadyConfigured = true,
}

local hs = {}

local ready = nil
if obj.status ~= nil and obj.status.conditions ~= nil then
  for _, condition in ipairs(obj.status.conditions) do
    if condition.type == "Ready" then
      ready = condition
    end
  end
end

if ready == nil then
  hs.status = "Progressing"
  hs.message = "Waiting for kflare to reconcile the resource"
  return hs
end

-- A paused resource keeps the status and generation of its last reconcile.
if ready.reason == "Paused" then
  hs.status = "Suspended"
  hs.message = ready.message
  return hs
end

if ready.observedGeneration ~= obj.metadata.generation then
  hs.status = "Progressing"
  hs.message = "Waiting for kflare to reconcile generation " .. tostring(obj.metadata.generation) ..
    " (last reconciled: " .. tostring(ready.observedGeneration) .. ")"
  return hs
end

if ready.status == "True" then
  hs.status = "Healthy"
elseif ready.status == "False" and needsPerson[ready.reason] then
  hs.status = "Degraded"
else
  hs.status = "Progressing"
end
hs.message = ready.message
return hs
