# Omni-Notify VOICE_CALL channel — Design Spec

**Date:** 2026-10-26
**Status:** Build (proof of concept, branch `voice-call`)

## Goal

Let an authorized AI agent, an Omni service or a scheduled task ring the owner's
iPhone with a real incoming call (PushKit + CallKit). When it is answered, Gobbie's
voice reads the notification aloud, and the owner can answer back: repeat, explain,
acknowledge, dismiss, snooze or reschedule. Voice is a **delivery channel**. It is not
a severity: a `info` reminder can ring and a `critical` alert can go to Discord.

## What was found (discovery, 2026-10-26)

| Asked for | Reality |
|---|---|
| Omni-Notify | Go service on chizu (:8088 behind a netviz sidecar, LAN-firewalled; `notify.omni.pod32g.dev` via Omni Access, enrolled device required). SQLite, events → routes → providers → durable retry queue. No APNs, no scheduling, no users or devices. One static admin bearer token. |
| Gobbie's voice infrastructure | **On the phone.** Gobbie is the presenter of the OmniWeather iOS app (`pod32g/OmniWeather`). Her voice is **Kokoro-82M v1.0 int8** (`kokoro-int8-multi-lang-v1_0`, 54 speakers, default `af_heart` = speaker 3), run by **sherpa-onnx 1.13.8 built from source against onnxruntime 1.28.2** (`tools/build-sherpa-ios.sh`), CPU, 2 threads, 24 kHz mono float. Sentence-by-sentence rendering. About 1.1× real time on an iPhone 17 Pro next to a 3D scene, with a 0.6 s load. A Core ML / Neural Engine variant (kokoro-coreml) loads in 35–39 s at every launch. There is no server-side inference service. |
| Gobbie STT / VAD | `SFSpeechRecognizer` on the device (`GobbieListening.swift`). End of utterance is 1.3 s of quiet after words, and it gives up after 7 s. No separate VAD. |
| Gobbie conversation | `RuleIntentResolver` in C#, deterministic. A Foundation Models resolver is planned, not built. |
| Omni-Oncall, K3s, local LLM, STT server | None exist. |
| Omni-Identity | OIDC. JWT access tokens (RS256 default, EdDSA also published) at `/jwks.json`. Supports `client_credentials`, where `sub` = `aud` = the client id. |
| Apple | Paid team `LVZHYJD66T`. `xcodebuild -allowProvisioningUpdates` mints Xcode-managed profiles, including `aps-environment`. **No APNs auth key (.p8) exists on this Mac.** |

## Decisions

1. **Voice runs on the phone, with Gobbie's exact engine.** The call app links the same
   sherpa-onnx 1.13.8 / onnxruntime 1.28.2 xcframeworks Gobbie built. It loads the same
   pinned model archive with the same configuration (`model.int8.onnx`, `voices.bin`,
   `tokens.txt`, `lexicon-us-en.txt`, `espeak-ng-data`, `lang=en-us`, `lengthScale=1`,
   `maxNumSentences=1`, `silenceScale=0.2`, 2 threads, `af_heart`). STT is the same
   on-device `SFSpeechRecognizer` with Gobbie's pause rules. **No audio leaves the
   phone.** So there is no WebRTC, no audio relay and no inference endpoint to protect,
   and the audio path is microphone → recogniser and synthesiser → speaker.
2. **Why a second copy of the model on the phone (the technical necessity).** iOS
   sandboxing prevents one app from loading another app's bundled files. Gobbie is a Unity app whose cold start (and 35–39 s Neural
   Engine load) cannot answer a PushKit wake within Apple's "report the call
   immediately" rule, and folding CallKit into it would put the Unity player in every call. So
   the call app carries the same 140 MB of model files, verified against Gobbie's pinned
   archive. Sharing them through an App Group with Gobbie is the way to drop the copy later.
   It needs an App Group on both App IDs and a change to Gobbie's build.
3. **Omni-Notify owns the call, the policy, the schedule and the dialogue.** The phone
   is audio input and output. Each thing the user says is posted as a *turn*. The server parses it
   with a deterministic, pluggable responder, validates and **persists** any action, and
   only then returns the reply to speak. A later Claude or GPT responder plugs in behind the same
   interface without touching transport.
4. **The calls table is the schedule.** A call is created `SCHEDULED` with
   `scheduled_at`. A dispatcher claims due calls by the atomic transition
   `SCHEDULED → RINGING` (single SQLite writer, conditional `UPDATE`), so restarts cannot
   ring twice. No in-memory timers. A sweeper moves calls whose ring deadline passed to `MISSED`,
   stale `ACTIVE` calls to `FAILED`, and calls past `expires_at` to `EXPIRED`. All of it
   is driven from timestamps in the database.
5. **First-class channel, two ways in.**
   - Explicit: `POST /api/v1/calls` (agents, services, admin, test mode). It stores an event for
     the audit trail but does not route it, so no unintended fan-out.
   - Policy: a `voice_call` provider kind, so any existing route can deliver an event by
     phone (for example a critical alert). The provider turns the delivery into a call for its
     configured recipient. Delivery success means the call was placed, and the call carries its own outcome.
6. **Identity.**
   - Agents: an Omni-Identity `client_credentials` JWT, verified against JWKS
     (iss, exp, `token_use=access`, client id → agent mapping in config), or a per-agent static
     token stored as a SHA-256 (the trusted service mapping, for local development).
     A request's `agent` field is checked against the authenticated agent and never trusted alone.
   - Devices: enrolled with a one-time code minted by an admin. They receive a device secret
     (stored hashed) and can only touch calls that rang them.
   - Admin: the existing tokens, unchanged.
   - A call made by admin `as_agent: claude` is marked **simulated**. A `test` call says so on
     screen and out loud.
7. **Signaling.**
   - The VoIP push (`apns-push-type: voip`, topic `<bundle>.voip`, priority 10,
     `apns-expiration` = ring deadline) carries only the call id, caller display name, caller id,
     test flag and expiry. It carries no message text and no credentials.
   - The app reports to CallKit synchronously, then fetches the call with its device secret.
   - A **poll transport** (foreground only, clearly marked as development) stands in for APNs
     until an APNs key exists.
8. **Latency.** While the phone rings, the app loads Kokoro and renders the opening
   sentences, so answering plays audio from memory.

## State machine

```
SCHEDULED ──▶ RINGING ──▶ ANSWERED ──▶ ACTIVE ──▶ COMPLETED
   │             ├──▶ DECLINED            │  └────▶ FAILED
   │             ├──▶ MISSED              └───────▶ COMPLETED | FAILED
   │             ├──▶ CANCELLED | FAILED
   ├──▶ CANCELLED
   └──▶ EXPIRED | FAILED
```

Every transition is validated against this table and recorded in `voice_call_log`. An
`outcome` (`acknowledged`, `dismissed`, `snoozed`, `rescheduled`, or empty) is separate from
the state: an answered call is not an acknowledged notification. A retry after `MISSED` and a snooze each create
a **new** call linked to the old one (`parent_call_id`), with the same event.

## Policy (config `voice:`)

These are per recipient: allowed agents, timezone, quiet hours with exceptions
(user-requested reminders and critical calls, each only for agents granted that
exception), calls per hour, a per-agent cooldown, retries after a missed call, the
longest snooze, an optional fallback provider (any existing provider) for calls that end
unanswered, and a cap on pending calls per agent. An agent cannot raise its own rights:
the quiet-hours exceptions are granted per agent in config, not claimed per request.
Snoozes and reschedules the user speaks on a call are user-requested by construction.

## Not in this PoC

- Recurring schedules: Omni-Notify has no recurrence infrastructure.
- Free-form LLM conversation: no local model service exists, and the responder is scripted.
- Executing infrastructure tools from a call: there is no such Omni service, and it is deliberately not allowed.

## Status (2026-10-26)

### Verified

- `go test -race ./...` passes; the existing suites are unchanged and still pass.
- Voice tests cover the state machine table, intent parsing, Gobbie's spoken-text rules,
  quiet hours and their exceptions, rate limit and cooldown, idempotency and duplicate
  suppression, missed → retry → fallback, decline (no retry), the first answer winning
  on two devices, restart safety, expiry, cancellation, no device, push failures, stale
  calls, enrollment codes, the APNs request over HTTP/2 (headers, ES256 provider token,
  minimal payload, 410/403 handling), Identity JWTs (RS256 and EdDSA; forged, expired,
  alg-swapped and user tokens are rejected), the HTTP principals, and the MCP protocol.
- **Live, the real binary on this Mac:**
  - Claude, through `omni-notify call` with a static agent token, placed a reminder.
  - A phone driven by curl answered, said "Give me another ten minutes", and the
    follow-up was persisted before the reply.
  - The server was restarted with the follow-up pending. It rang at 15:40:23, 0.5 s
    after its time, and exactly once.
  - The unanswered follow-up was retried once (attempt 2), then stopped.
  - A call whose app died mid-call was failed by the sweeper after 5 minutes of no
    activity.
- **Gobbie's voice, same model files and configuration, on this Mac** (macOS sherpa-onnx
  1.13.8):
  - 54 speakers, 24 kHz, non-silent output; loads in about 0.55 s; renders at 1.4–1.9×
    real time.
  - A long first sentence took 1.7–1.85 s to render. Replies now open with a short
    phrase that is rendered ahead, so they start without waiting.
- **The iOS app, iOS 27 Simulator** (CallKit bypassed with `-OmniNoCallKit`, see below):
  - Enrolled by code; poll transport.
  - Kokoro loaded in 0.64–0.90 s; the opening's first sentence was ready 1.4–1.65 s after
    the call arrived.
  - With 3–5 s of ringing, first audio came 21–100 ms after answering.
  - Turns took 2–7 ms on the server. Replies starting with a phrase rendered ahead were
    heard 3–8 ms after the turn was sent.
  - Snooze, explain and reschedule (to 17:30 local) were persisted and confirmed.
  - The silence path ended the call politely.
  - The simulator caught a crash (an input tap at a stale sample rate); fixed by building
    the audio engine when the call's audio session activates.

- **Gobbie's voice on Suzuka** (iPhone 17 Pro, the app's "Hear Gobbie's voice" test):
  Kokoro loaded in 0.56 s; the first sentence rendered in 0.94 s; 3.3 s of speech
  rendered in 2.28 s (about 1.45× real time).
- **A real call on Suzuka, as Claude** (static agent token, poll transport, app in front):
  - It rang through CallKit as "Claude" and was answered on the phone. Gobbie's voice
    started 435–444 ms after answering.
  - Apple's on-device recognizer heard "Hello" (an unknown intent, so "Sorry, I didn't
    catch that…") and then "OK". The acknowledgement was persisted before "Got it. Bye.",
    and the call ended as `completed / acknowledged`.
  - End of speech to text took about 1.05–1.1 s, almost all of it the 1.0 s pause rule.
    Turns took 19–32 ms, and replies began 19–32 ms after the turn was sent.
  - The first call heard nothing: the recognizer returned at once after the opening, and
    the error was not logged then. The app now logs recognizer errors and permissions.
    It also logged speech recognition as not authorized while on-device recognition
    worked.
- **Two device-only failures, fixed:**
  - The first device build would not launch: a Debug build puts the app's code in
    `OmniNotify.debug.dylib`, which the hand-signing step left unsigned (dyld: "Library
    missing … missing code signature"). `build-device.sh` now sets
    `ENABLE_DEBUG_DYLIB=NO` and signs nested code first.
  - Enrollment at launch failed with "The Internet connection appears to be offline".
    iOS reports a request it blocks for want of Local Network permission that way, and
    the server never saw it. The app now explains this and retries the enrollment when it
    comes back to the front, for example right after the Local Network prompt.

### Simulated or bypassed

- **CallKit in the Simulator.** The iOS 27 Simulator has no in-call UI, so CallKit
  disconnects every incoming call ("there wont be a UI to host the call").
- **Speech recognition in the Simulator.** It does not run, so replies were scripted
  (`-OmniScriptedReplies`).
- **Claude's identity.** Claude was a static agent token, not an Omni-Identity client: no
  Identity client exists yet.

### Pending (needs the owner)

- **VoIP push on a phone.** It needs a provisioning profile with Push Notifications for
  `dev.pod32g.OmniNotify` (Xcode has no account signed in, so automatic signing cannot
  make one), and an APNs auth key (.p8) for the server. Until then a phone rings only
  through the foreground poll transport.
- **The speaker on a call (parked 2026-10-26).**
  - Calls open with a greeting ("Hi, this is Gobbie with a reminder from Claude."). The
    phone then waits up to 3 s, or until the call moves off the earpiece, before the
    message.
  - Switching to the speaker works as far as iOS reports: the route is Speaker, and the
    app builds a new audio engine and replays unheard sentences. The app plays at about
    −8 dBFS, and the microphone and recognizer work afterwards.
  - The owner heard the voice only from the top earpiece, not the bottom speaker. The
    likely cause is plain I/O feeding only the earpiece side of the stereo speaker pair.
  - Installed but not verified: calls now use iOS voice-processing I/O
    (`setVoiceProcessingEnabled`), and the log records output channels on every engine
    build. The automatic test (`-OmniAutoAnswer 3 -OmniSpeakerAfter 1.2`) was answered,
    but its log was not read before the owner parked the work.
- **The rest of the phone run.** Still to try on Suzuka: a spoken snooze ("give me another
  ten minutes") and its call back, declining, ringing out, and a locked phone. A locked
  phone needs VoIP push.
- **Production.**
  - Deploying to chizu (deploys paused, and a push to `main` deploys).
  - An Omni-Identity client per agent.
  - A way for phones to reach Omni-Notify: `notify.omni.pod32g.dev` requires an enrolled
    device through Omni Access, which this app is not, and :8088 is LAN-firewalled.
