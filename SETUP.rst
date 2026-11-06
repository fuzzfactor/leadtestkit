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
3. Create a branch named ``leadtestkit`` with only the workflows you
   want LeadTestKit to start for every patch, for example::

     git switch --orphan leadtestkit
     mkdir -p .github/workflows
     cp <leadtestkit>/fork-workflows/checkpatch-ovs.yml .github/workflows/
     git add .github && git commit -m "LeadTestKit workflows."
     git push origin leadtestkit

   Workflows must be started by ``workflow_dispatch``, take the inputs
   used in the examples and set ``run-name: leadtest ${{
   inputs.correlation_id }}``.  If you do not want custom workflows,
   the branch can stay empty.
4. Make ``leadtestkit`` the default branch of the fork.  GitHub only
   starts ``workflow_dispatch`` workflows that exist on the default
   branch, and scheduled upstream workflows stop running in the fork.

Patches are pushed to ``series_<id>`` branches, which runs the
project's own workflows on push.  The full series is also kept in
``refs/leadtest/series_<id>``.


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

To go live, set ``checks: post`` and ``email: live``.  Setting
``enabled: false`` pauses the bot.


5. Day to day
-------------

The state is plain JSON on the ``state`` branch:

- ``queue.json``: series that are not finished;
- ``history/YYYY-MM.json``: finished series;
- ``meta.json``: event and archive cursors.

Every change is a commit, so ``git log -p`` on the branch shows what
the bot did.  You can edit the files and push; the bot rebases over
your changes.  Useful fields on a series:

- ``"hold": true`` keeps it from being pushed;
- ``"skip": "reason"`` stops testing it;
- ``"retry": true`` retries failed operations, or tests a finished
  series again;

and on a patch:

- ``"recheck": true`` reruns its workflows;
- ``"retry": true`` retries failed operations.

The workflow can also be started manually with *Run workflow*, for a
single step or a dry run.

Concurrency is controlled in ``limits``: patches are pushed while
fewer than ``max_queued_jobs`` jobs wait for a runner and fewer than
``max_inflight_patches`` patches are being tested.  A single author
never gets more than ``per_author_max_share`` of the in-flight patches
while others are waiting.  The pages and the job summary show the
queue, so the limits can be tuned from real data.
