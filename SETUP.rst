Setting up a LeadTestKit instance
=================================

Every project runs its own instance from its own copy of leadtestkit,
so runner limits and configuration are not shared.  You need:

- a copy of leadtestkit (the *bot repository*), for example
  ``ovs-ci/leadtestkit``;
- a copy of the project repository to run the tests in (the *test
  repository*), for example ``ovs-ci/ovs``, preferably in a separate
  organization so it gets its own runner concurrency;
- a Patchwork account with an API token, if checks are posted;
- an SMTP account allowed to post to the reports list.

Both can be forks on GitHub, but standalone repositories are better:
GitHub disables Actions on forks more readily, and forks start with
scheduled workflows disabled.  To turn an existing fork into a
standalone repository, use *Leave fork network* in the Danger Zone of
its settings; it keeps its name, branches and history.


1. Prepare the test repository
------------------------------

The test repository runs code from anyone who sends a patch to the list.
**Do not store any secrets in it.**

1. Create an empty public repository, and push the default branch and
   tags of the project to it::

     git clone --bare https://github.com/openvswitch/ovs.git
     git -C ovs.git push https://github.com/ovs-ci/ovs.git main --tags

   A fork works too.  Keep the repository public: GitHub Actions are
   free for public repositories only.
2. Enable GitHub Actions in it.
3. Add the workflows you want LeadTestKit to start for every patch to
   its ``main`` branch (the *tools branch*), on top of upstream, for
   example::

     git switch main
     cp <leadtestkit>/fork-workflows/leadtest-checkpatch-ovs.yml .github/workflows/
     $EDITOR Makefile.am
     git add .github Makefile.am && git commit -m "LeadTestKit workflows."
     git push origin main

   Open vSwitch and OVN check in every build that all files in git are
   distributed, so list the new files in ``EXTRA_DIST`` in
   ``Makefile.am``, next to the existing ``.github/workflows/`` entries.
   Otherwise the project's workflows fail on ``main``, including the
   runs after the weekly sync that fill the caches.  If an upstream
   change near these lines makes the sync conflict, rebase by hand as
   described below.

   File names must start with ``leadtest-``; the project's own
   workflows on the branch are never started by the bot.  Workflows
   must be started by ``workflow_dispatch``, take the inputs used in
   the examples and set ``run-name: leadtest ${{
   inputs.correlation_id }}``.  Reports and checks use the workflow's
   ``name:``, like ``Checkpatch``, not the run name.  Without custom
   workflows, leave ``main`` as it is.
4. Keep the tools branch, ``main``, the default branch.  GitHub only
   starts ``workflow_dispatch`` workflows from the default branch, and
   the runs on test branches can only restore caches saved on their own
   branch or on the default branch.  Every week
   (``test_repo.sync_interval``), LeadTestKit rebases the commits of
   the tools branch that are not upstream onto upstream's default
   branch, ``upstream.default_branch``, like ``git pull --rebase``, and
   force-pushes it; the project's workflows then run on it and fill the
   caches.  The tools branch stays upstream with your commits on top;
   merge commits of earlier syncs are dropped.  If the sync reports a
   conflict, rebase it by hand::

     git pull --rebase https://github.com/openvswitch/ovs.git main
     git push --force-with-lease origin main

Patches are pushed to ``series_<id>`` branches, which runs the
project's own workflows on push.  The applied series and patches are
also kept in ``refs/leadtest/series_<id>`` and
``refs/leadtest/patch_<id>``, which do not start workflows; they let
the bot recover without running anything twice if it loses its state.

The workflows of a tested patch can write to the test repository, so
protect its branches once the App of step 2 exists, as described in
`SECURITY.rst <SECURITY.rst>`_:

1. Settings, Rules, Rulesets, *New ruleset*, *New branch ruleset*.
2. **Ruleset name**: ``leadtest``; **Enforcement status**: *Active*.
3. **Bypass list**: *Add bypass*, the App of step 2, and the
   *Repository admin* role for maintaining ``main`` by hand.
4. **Target branches**: *Add target*, *Include all branches*.
5. **Rules**: *Restrict creations*, *Restrict updates*, *Restrict
   deletions* and *Block force pushes*.

Also set Settings, Actions, General, **Workflow permissions** to *Read
repository contents and packages permissions*, and keep the repository
free of secrets, environments and self-hosted runners.


2. Create a GitHub App for the test repository
----------------------------------------

LeadTestKit works on the test repository as a GitHub App installed on
that repository only.  The App needs no server and no webhook: it is an
identity with permissions.  For every run the bot signs a request with
the App's private key and gets a token for the test repository that is
valid for an hour, so there is no token to renew.  Changes in the test
repository show up as made by ``<app name>[bot]``.

Create the App in the account that owns the test repository.  An App
limited to its own account can only be installed there, so an App
created in your user account cannot be installed in an organization.

- Test repository in your user account: open
  https://github.com/settings/apps/new (Settings, Developer settings,
  GitHub Apps, *New GitHub App*).
- Test repository in an organization, here ``<org>``: open
  ``https://github.com/organizations/<org>/settings/apps/new``.  The
  list of the organization's own Apps is at
  ``https://github.com/organizations/<org>/settings/apps``; in the
  settings menu it is Developer settings, GitHub Apps.  Do not confuse
  it with Third-party access, GitHub Apps
  (``.../settings/installations``), which lists Apps installed in the
  organization, not Apps it owns.  Creating Apps needs an organization
  owner or a member an owner made *App manager* (on the
  ``.../settings/apps`` page).

Fill in the form:

- **GitHub App name**: any name not taken on GitHub; names are global,
  so include your project, for example ``ovs-leadtestkit``.
- **Homepage URL**: required but unused, for example the URL of the
  bot repository.
- **Webhook**: clear *Active*.
- **Repository permissions**:

  - Actions: read and write (reading runs, reruns, dispatching);
  - Checks: read-only (warning annotations, see
    ``workflows.warning_annotation`` in ``examples/reference.yaml``);
  - Contents: read and write (pushing test branches);
  - Workflows: read and write (patches may change workflow files);
  - Metadata: read-only (selected automatically).

- **Where can this GitHub App be installed?**: *Only on this account*.

Then *Create GitHub App* and, on the App's settings page:

1. Note the **Client ID** at the top (``Iv23...``).  The App ID shown
   next to it works as well.  The App needs no client secret.
2. Under *Private keys*, *Generate a private key*.  A ``.pem`` file is
   downloaded; it goes into a secret below.  Do not keep other copies.
3. In the left menu, *Install App*, choose the account, then *Only
   select repositories* and the test repository.  The App must not be
   installed on other repositories.

LeadTestKit logs the permissions of every token it gets and warns about
missing ones.  After changing the App's permissions, accept the change
on the installation (the account's settings, Applications or
Third-party access, GitHub Apps, *Configure*), or the tokens keep the
old permissions.  To replace the private key, generate a new one,
update the secret and delete the old key in the App settings.


3. Configure the bot repository
-------------------------------

1. Copy leadtestkit into a new repository, or fork it and leave the
   fork network, and enable GitHub Actions, including scheduled
   workflows, which forks have disabled.  GitHub often starts scheduled
   workflows late; `SCHEDULER.rst <SCHEDULER.rst>`_ explains how to
   start the bot from a scheduler elsewhere.
2. Add the repository secrets (Settings, Secrets and variables,
   Actions):

   ============================  ==========================================
   ``LEADTEST_APP_CLIENT_ID``    Client ID (or App ID) from step 2
   ``LEADTEST_APP_PRIVATE_KEY``  the whole ``.pem`` file from step 2,
                                 including the BEGIN and END lines
   ``LEADTEST_PW_TOKEN``         Patchwork API token, needed to post checks
   ``LEADTEST_SMTP_USERNAME``    SMTP user name
   ``LEADTEST_SMTP_PASSWORD``    SMTP password
   ============================  ==========================================

   The App secrets are needed when ``mode.push`` is ``live``.  Without
   them the public test repository is read anonymously.

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
    push: live         # push to the test repository
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
- ``meta.json``: event and archive cursors, and ``outages`` while
  something the bot needs does not work (see below).

Finished series and patch records are retained indefinitely.  History
lookups are also unlimited by default (``limits.history_window: 0``).
Set a duration to restrict lookups to recent history.  This does not
delete records or change ``rechecks.window`` or branch cleanup.

Test branches and recovery refs become eligible for cleanup 730 days
after a series finishes.  Set ``limits.branch_retention`` to change
this period, or to ``0`` to keep them indefinitely.

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

When something stops working
~~~~~~~~~~~~~~~~~~~~~~~~~~~~

When a service the bot needs does not work, the failures are not the
patches' fault.  The bot then keeps the patches waiting instead of
giving up on them after ``limits.retry_deadline``, reports the problem
in the job summary of every run and at the top of the pages, and
continues by itself once it works again.  It recognizes:

- **Workflows that do not run on the test repository.**  GitHub sometimes
  disables Actions on forks: pushes then start no workflows and
  dispatches fail with *Actions has been disabled for this repository*,
  while the repository settings still show Actions as enabled.  Only
  GitHub Support can undo it.  The bot notices the refused dispatches,
  or a patch that got no run at all for ``limits.no_run_timeout`` while
  nothing else ran on the test repository either.  It then pushes no new
  patches and sends no warnings for patches without runs.  Every hour
  it pushes one patch to check whether workflows run again; patches
  that got no runs wait to be pushed again then.
- **GitHub refusing the GitHub App**: the App was uninstalled or its
  credentials are wrong.  Nothing is pushed or monitored until GitHub
  accepts it again, which the bot checks in every cycle.
- **Email that cannot be sent for an hour**: the SMTP server cannot be
  reached or refuses the login.  Reports wait and are sent once it
  works again.
- **An archive that cannot be read for a day**: final Patchwork checks,
  or rechecks from the dev list, wait until it can be read again.

The maintainers in ``alerts.to`` get an email when the bot notices a
problem, once a day while it lasts and when it is over.  The bot runs
that send these emails fail, which makes them easy to find.  When email
itself does not work for an hour, these runs fail without sending
anything.  Without ``alerts.to``, only the job summaries and the pages
show the problems.  Outages of a service that gets turned off, like
email or reading an archive, are forgotten.

The bot cannot notice that it does not run itself, for example when
GitHub disables Actions on the bot repository.  The pages warn when
the bot has not updated them for ``pages.stale_after``, and say when
its last run failed; for notifications, see `SCHEDULER.rst
<SCHEDULER.rst>`_.


6. Upgrading
------------

To upgrade, pull the upstream leadtestkit repository into your copy
and push it, or use *Sync fork* while it is a fork.
Every state file records its ``schema`` version, and a newer leadtest
upgrades older files automatically as it reads them.  An older leadtest
refuses state written by a newer one with a clear error, so roll back
the state branch too if you roll back the code.

``meta.json`` records the Patchwork instance, project and test
repository the state belongs to.  If ``config.yaml`` changes any of
them, the bot stops with an error instead of mixing up two instances;
update or remove ``"instance"`` in ``meta.json`` if the change is
intended.
