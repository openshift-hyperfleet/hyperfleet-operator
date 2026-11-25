# Konflux build failure notifications

These PipelineRuns notify the HyperFleet team's `#hyperfleet-e2e-status` Slack
channel when a completed build fails: `hyperfleet-operator-on-push`,
`hyperfleet-operator-on-tag`, `hyperfleet-operator-bundle-on-push`, and
`hyperfleet-operator-fbc-on-tag`.

Each inline pipeline has a `slack-webhook-notification` final task guarded by
`$(tasks.status) in ["Failed"]`. The message names the pipeline and links the
repository, commit, and failed run. Successful, cancelled, and
pre-start/admission-failed runs do not trigger this build notification. This is
separate from release notifications.

The catalog Task reads the `hyperfleet-slack-webhook-url` key from the
`hyperfleet-slack-webhook-notification-secret` Secret in the `hyperfleet-tenant`
namespace. HyperFleet manages the Secret and destination channel; confirm its
availability to build service accounts before rollout. The webhook value must
stay out of Git, logs, and review comments. See the
[notification operations runbook](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/docs/release/operations/notifications.md)
for release notification ownership.

## Validate and troubleshoot

After an authorized controlled failed build, check that its final TaskRun
succeeded, that the Slack message arrived within a few minutes, and that the
repository, pipeline, commit, and run link match the PipelineRun. Then observe
an authorized successful build and confirm it produced no **build failure**
message. A release notification in the same channel is separate. If delivery
fails, inspect the PipelineRun's final TaskRun logs and confirm that the Secret
and named key exist in `hyperfleet-tenant` and are accessible to the build
service account. The image and tag definitions use
`build-pipeline-hyperfleet-operator`; bundle and catalog use their respective
`build-pipeline-hyperfleet-operator-bundle` and
`build-pipeline-hyperfleet-operator-fbc` accounts. Do not print the Secret
value while diagnosing delivery.

## Rotate the webhook

1. Coordinate a replacement webhook for the approved channel with the
   HyperFleet team.
2. Update the build tenant Secret through the team's Secret management process
   and its configuration source if one is used. If the same webhook is used by
   release notifications, coordinate the release source update with RelEng.
3. Confirm Secret synchronization using metadata and key presence only. Run
   an authorized controlled failure to verify the new build alert, plus a
   release notification if the webhook is shared. Confirm an authorized
   successful build emits no build failure alert.
4. Revoke the old webhook only after every consumer has been verified on the
   replacement.
