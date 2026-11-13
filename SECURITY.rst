Security model
==============

LeadTestKit tests patches from anyone who can send email to a mailing
list.  Their code runs on GitHub's runners, so the design keeps the
code of tested patches away from every credential, and treats
everything that code can touch as untrusted.

This page describes where code runs, what each credential can do, what
a hostile patch can and cannot do, and the remaining risks.


Where code runs
---------------

=====================  ===============================  ================================
Place                  Code that runs there             Credentials available
=====================  ===============================  ================================
Bot repository job     LeadTestKit, git, the actions    all LeadTestKit secrets, the
                       in ``leadtest.yml``; never code  job's ``GITHUB_TOKEN``
                       from patches
Test repository        anything a patch contains,       only the test repository's own
workflows              including its workflow files     ``GITHUB_TOKEN``
External scheduler     the scheduler code from          credentials that can start
(optional)             SCHEDULER.rst                    workflows in the bot repository
=====================  ===============================  ================================

The bot job only *applies* patches, with ``git am``, and never builds
or runs them.  Global and system git configuration are disabled, so no
hooks, filters or other configured commands run.  Every process in the
job runs as the same user and could read the secrets of the others, so
nothing untrusted may ever run there; the workflow has no
``pull_request`` or ``pull_request_target`` trigger.  ``ci.yml`` runs
on pull requests to the bot repository with a read-only token and no
secrets.


Credentials
-----------

``GITHUB_TOKEN`` of the bot repository
  Created by GitHub for each job of ``leadtest.yml``.  The ``run`` job
  may write contents (the ``state`` branch); the ``pages`` job may
  deploy GitHub Pages.  Valid for the job only.

``LEADTEST_APP_CLIENT_ID`` and ``LEADTEST_APP_PRIVATE_KEY``
  Repository secrets of the bot repository.  The key lets anyone who
  has it act as the App on every repository the App is installed on,
  with all its permissions, until the key is deleted in the App
  settings.  Install the App on the test repository only, and never in the
  bot repository.

Installation token
  Created by the bot from the App key for every run, valid for an hour,
  limited to the test repository and to the permissions the bot uses:

  - Actions: read and write (reading runs, reruns, dispatching);
  - Checks: read (warning annotations);
  - Contents: read and write (test branches and recovery refs);
  - Workflows: read and write (patches may change workflow files);
  - Metadata: read (always granted).

  It is sent only to the configured GitHub API host and, for git, only
  to the test repository, through environment variables rather than URLs,
  arguments or files.  It never reaches the test repository's workflows.

``LEADTEST_PW_TOKEN``
  A Patchwork API token, with every right of its Patchwork user.  The
  bot only posts checks, but Patchwork lets only users who may edit a
  patch post checks to it, so the account usually has to be a
  maintainer of the project, and the token could also change patch
  states and delegates.  Use a dedicated account for the bot.

``LEADTEST_SMTP_USERNAME`` and ``LEADTEST_SMTP_PASSWORD``
  Allow sending email as the bot, from the address in the user name.
  In test mode, emails only go to ``mode.allowed_recipients``.

Scheduler credentials
  A fine-grained personal access token or a separate GitHub App, with
  Actions: read and write on the bot repository only (see
  SCHEDULER.rst).  They can start, rerun and cancel the bot's
  workflows, but cannot read secrets or push.  A token acts as the
  person who created it; an App does not.

The secrets are only given to the step that runs ``leadtest`` and are
redacted from its logs: the App key line by line, and every token the
bot creates.  Anyone with write access to the bot repository can
change the workflow and read every secret, so only give that access to
the people running the instance.


The test repository
-------------

The test repository runs arbitrary code by design.  It must hold no
secrets, no environments with secrets and no self-hosted runners: anything
there is readable by every tested patch, and a self-hosted runner
would run that code on your machine.

The workflows of a tested patch, and the workflows a patch adds or
changes, get the test repository's ``GITHUB_TOKEN``.  On pushes a
workflow can ask for write permissions with ``permissions:``, whatever
the repository's default, so a hostile patch can use the GitHub API on
the test repository.  What it could do, and what limits it:

Push to branches
  Changing ``main`` would change the workflows the bot dispatches for
  every later patch, and moving other ``series_*`` branches would
  confuse their tests.  The ruleset from SETUP.rst, step 1, lets only
  the App and administrators create, update or delete branches.  Pushes
  with a ``GITHUB_TOKEN`` do not start workflows in any case.

Rewrite recovery refs
  Rulesets cannot protect ``refs/leadtest/*``.  When the bot reuses
  saved commits after losing its state, it checks that every commit
  makes the change of its patch (same ``git patch-id``) and applies the
  patch again otherwise, recording ``recovery-rejected`` in the patch's
  decision history.

Dispatch workflows
  A ``GITHUB_TOKEN`` can start ``workflow_dispatch`` workflows, which
  could imitate the runs the bot starts for other patches.  The bot
  only accepts dispatched runs started by its App.

Cancel or rerun other runs
  A cancelled run is rerun once and then reported as a warning; a rerun
  repeats the same commit.  This can delay or disturb results, but not
  turn a failure into a success.

Create check runs and annotations
  The bot turns a successful run into a warning when one of its jobs
  has a ``leadtest`` warning annotation.  A hostile patch that manages
  to add annotations to another patch's checks could only add a
  warning, not hide a failure.

Use runners
  A patch can use the test repository's runners for as long as GitHub
  allows.  The bot limits how many patches it tests at once and per author
  (``limits``), and maintainers can hold or skip series in the state.

A hostile patch cannot read any LeadTestKit secret, change the bot
repository or its state, send email, or post Patchwork checks.


Other inputs
------------

Patchwork and the mailing lists are trusted to the same degree as by
existing tools:

- Anyone who can post to the reports list can make the bot post a
  check for any patch when ``archive.ingest`` is enabled, under any
  test label.  Moderating the reports list limits this.
- Anyone who can reply on the development list can request rechecks,
  which only rerun workflows of patches tested within
  ``rechecks.window``.
- Patch names, log excerpts and report texts are untrusted.  Email
  headers are encoded, so they cannot add headers, and the pages show
  all text as text, never as HTML.


Recommended setup
-----------------

- Run the test repository in its own organization or account, with no
  secrets, environments or self-hosted runners.
- Add the branch ruleset and read-only default workflow permissions
  from SETUP.rst, step 1.
- Install the test repository's App on the test repository only; use a
  separate App or token for the scheduler.
- Use a dedicated Patchwork account for the bot, with no more rights
  than posting checks needs.
- Limit write access to the bot repository to the people running it.
- If the App key may have leaked, generate a new one, update the
  secret and delete the old key in the App settings; tokens made from
  the old key expire within an hour.
