Setting up a LeadTestKit instance
=================================

Every project runs its own instance from its own fork of leadtestkit,
so runner limits and configuration are not shared.  You need:

- a fork of leadtestkit (the *bot repository*), for example
  ``ovs-ci/leadtestkit``;
- a fork of the project repository to run the tests in (the *test
  fork*), for example ``ovs-ci/ovs``, preferably in a separate
  organization so it gets its own runner concurrency;
- a Patchwork account with an API token, if checks are posted;
- an SMTP account allowed to post to the reports list.


1. Prepare the test fork
------------------------

The test fork runs code from anyone who sends a patch to the list.
**Do not store any secrets in it.**

1. Fork the project repository.
2. Enable GitHub Actions in the fork.
3. Add the workflows you want LeadTestKit to start for every patch to
   the fork's ``main`` branch (the *tools branch*), on top of upstream,
   for example::

     git switch main
     cp <leadtestkit>/fork-workflows/leadtest-checkpatch-ovs.yml .github/workflows/
     git add .github && git commit -m "LeadTestKit workflows."
     git push origin main

   File names must start with ``leadtest-``; the project's own
   workflows on the branch are never started by the bot.  Workflows
   must be started by ``workflow_dispatch``, take the inputs used in
   the examples and set ``run-name: leadtest ${{
   inputs.correlation_id }}``.  Without custom workflows, leave
   ``main`` as it is.
4. Keep ``main`` the default branch of the fork.  GitHub only starts
   ``workflow_dispatch`` workflows from the default branch, and the
   runs on test branches can only restore caches saved on their own
   branch or on the default branch.  LeadTestKit syncs ``main`` with
   upstream every week (``test_repo.sync_interval``) with GitHub's
   *Sync fork*; the project's workflows then run on ``main`` and fill
   the caches.  If the sync reports a conflict, merge upstream into
   ``main`` by hand.

Patches are pushed to ``series_<id>`` branches, which runs the
project's own workflows on push.  The applied series and patches are
also kept in ``refs/leadtest/series_<id>`` and
``refs/leadtest/patch_<id>``, which do not start workflows; they let
the bot recover without running anything twice if it loses its state.


2. Create the token for the test fork
-------------------------------------

Create a fine-grained personal access token (or a GitHub App token)
with access to the test fork only:

- Contents: read and write (pushing test branches);
- Workflows: read and write (patches may change workflow files);
- Actions: read and write (reading runs, reruns, dispatching).

Fine-grained tokens expire; LeadTestKit warns in the job log two weeks
before the expiration date.


3. Configure the bot repository
-------------------------------

1. Fork leadtestkit and enable GitHub Actions, including scheduled
   workflows (forks have them disabled by default).
2. Add the repository secrets (Settings, Secrets and variables,
   Actions):

   ==========================  ==========================================
   ``LEADTEST_GITHUB_TOKEN``   token from step 2
   ``LEADTEST_PW_TOKEN``       Patchwork API token, needed to post checks
   ``LEADTEST_SMTP_USERNAME``  SMTP user name
   ``LEADTEST_SMTP_PASSWORD``  SMTP password
   ==========================  ==========================================

3. In Settings, Pages, set the source to *GitHub Actions*.
4. Create the ``state`` branch with your configuration::

     git switch --orphan state
     cp examples/ovs.yaml config.yaml
     $EDITOR config.yaml
     git add config.yaml && git commit -m "LeadTestKit configuration."
     git push origin state

   Without a ``state`` branch with a ``config.yaml``, the workflow does
   nothing.  ``leadtest validate --state <dir>`` checks the
   configuration locally.

All non-secret settings live in ``config.yaml``; the bot never changes
it.  ``examples/reference.yaml`` lists every option with its default
value and an explanation.


4. Test mode
------------

The example configurations start in test mode:

.. code-block:: yaml

  mode:
    push: live         # push to the test fork
    checks: log        # only log the checks that would be posted
    email: test        # mark reports and send only to allowed_recipients
    allowed_recipients:
      - ovs-build@openvswitch.org

Test reports have ``[leadtest-dryrun]`` in the subject, rename the
``Test-Label:`` and ``Test-Status:`` markers and write the patch link
without a scheme, so other bots reading the list ignore them.  They
never copy the patch author or anyone else, whatever the rest of the
configuration says.

On its first run the bot only records where Patchwork and the archives
are; series and reports from before that are not processed.  To get
something to test right away, set ``limits.initial_backfill``, for
example to ``1d``, before the first discovery.  The cutoff is the later
of the saved discovery start minus this backfill and the current time
minus ``limits.series_max_age``.

Backfill is currently bounded: the initial event scan reads at most
three pages, and a safety scan checks only the newest 50 series once
per invocation.  It is not an exhaustive historical search.  Once
``meta.json`` has a ``series-completed`` event cursor, event discovery
continues after it; increasing ``initial_backfill`` does not rewind
that cursor.  Only the newest-series safety scan uses the revised
cutoff.  Archive initialization is separate and never backfills old
reports based on this setting.

To go live, set ``checks: post`` and ``email: live``.  Setting
``enabled: false`` pauses the bot.


5. Day to day
-------------

The state is plain JSON on the ``state`` branch:

- ``queue.json``: series that are not finished;
- ``history/YYYY-MM.json``: finished series;
- ``meta.json``: event and archive cursors.

Finished series and patch records are retained indefinitely.  History
lookups are also unlimited by default (``limits.history_window: 0``).
Set a duration to restrict lookups to recent history.  This does not
delete records or change ``rechecks.window`` or branch cleanup.

Every change is a commit, so ``git log -p`` on the branch shows what
the bot did.  You can edit the files and push; the bot rebases over
your changes.  Useful fields on a series:

- ``"hold": true`` keeps it from being pushed;
- ``"skip": "reason"`` stops testing it;
- ``"retry": true`` retries failed operations, or tests a finished
  series again, applied anew on the current base;

and on a patch:

- ``"recheck": true`` reruns its failed workflows (all of them with
  ``workflows.rerun_jobs: all``);
- ``"recheck_checks": ["github-robot-_Build_and_Test"]`` reruns the runs
  with these Patchwork check names, like a ``Recheck-request:`` with
  the check name;
- ``"retry": true`` retries failed operations.

The workflow can also be started manually with *Run workflow*, for a
single step or a dry run.

Concurrency is controlled in ``limits``: patches are pushed while
fewer than ``max_queued_jobs`` jobs wait for a runner and fewer than
``max_inflight_patches`` patches are being tested.  A single author
never gets more than ``per_author_max_share`` of the in-flight patches
while others are waiting.  The pages and the job summary show the
queue, so the limits can be tuned from real data.

Reading the logs
~~~~~~~~~~~~~~~~

Open the workflow run in GitHub Actions and expand the step running
``leadtest``.  Each polling cycle ends with a queue summary and totals
for the run.  Failed operations include their next retry time.

Expand a series on Pages and open its *Decision history*, or the
history under an individual patch.  Each entry includes a timestamp,
the decision or action, and a *bot run logs* link to the GitHub Actions
run attempt that handled it.  The same entries are stored in the
``decisions`` arrays in ``queue.json`` and history files.  Links are
recorded when the bot runs in GitHub Actions; local runs have no link.
Older records have no decision history until the bot next acts on them.
Linked logs are subject to the repository's Actions retention settings.

If no tests start, check the logs for:

- the discovery cutoff and saved event cursor, especially when using
  ``limits.initial_backfill``;
- series skipped by eligibility checks or paused by a manual hold;
- push mode, which must be ``live`` to schedule tests;
- retry delays, missing dependencies, or CI capacity limits.

Single-step runs do not schedule new patches.  Use a normal run to
start tests from the queue.

For git command details and timings, pass ``--debug`` to
``leadtest run``.


6. Upgrading
------------

To upgrade, sync your fork with the upstream leadtestkit repository.
Every state file records its ``schema`` version, and a newer leadtest
upgrades older files automatically as it reads them.  An older leadtest
refuses state written by a newer one with a clear error, so roll back
the state branch too if you roll back the code.

``meta.json`` records the Patchwork instance, project and test
repository the state belongs to.  If ``config.yaml`` changes any of
them, the bot stops with an error instead of mixing up two instances;
update or remove ``"instance"`` in ``meta.json`` if the change is
intended.
