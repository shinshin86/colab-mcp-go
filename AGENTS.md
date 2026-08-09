# Project Agent Instructions

## Live Google Colab Verification

- Do not use the Codex in-app browser for Colab verification that depends on Google Account authentication. Unless the user explicitly requests another browser, use the user's external Chrome session so the user can control and verify the signed-in account.
- Before opening Colab, explain which browser and notebook will be used and which operations are planned, then obtain the user's explicit approval. Do not sign in, sign out, or switch Google Accounts on the user's behalf.
- Do not interpret a general request such as "test this in Colab" as permission to allocate a runtime or execute code. Perform checks that do not require a runtime first, including local tests and inspection of the live Colab tool names and schemas.
- Immediately before allocating a runtime or executing code, explain the following and obtain the user's explicit approval:
  - Whether the intended Google Account is signed in
  - The runtime type and hardware accelerator that will be used
  - The possibility of consuming free-tier limits or paid compute units
  - The current plan for the runtime after the requested work is complete
- Do not select a GPU, TPU, or high-memory runtime unless the user explicitly requests it. A CPU runtime also requires approval before allocation.
- Runtime lifecycle decisions belong to the user. Do not disconnect, restart, delete, or unassign a runtime merely because verification or another task has finished, because the runtime appears idle, or because releasing it seems economical.
- At the end of work, report whether a runtime was allocated, whether code was executed, and the runtime's known state. Ask the user whether to keep, disconnect, or delete the runtime. If the user has not made that decision, leave the runtime unchanged.
- Disconnect, delete, restart, or unassign a runtime only after the user explicitly authorizes that exact action for the current runtime. Immediately before a destructive runtime action, restate that in-memory state and files outside mounted Drive may be lost.
- If the purpose of the test is to verify runtime disconnection or deletion, inspect the live Colab tool schemas first and explain the expected effects before requesting action-time approval. If the mechanism is unverified, fails, or returns an unknown outcome, do not improvise another destructive cleanup action or retry automatically; report the result and let the user decide what to do next.
- Do not mount Google Drive, modify an existing notebook, or save a notebook without explicit permission. Prefer a temporary scratchpad when practical.
- Never expose connection tokens, account identifiers, or other authentication details in logs, committed files, or user-facing reports.
