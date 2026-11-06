LeadTestKit
===========

A Patchwork CI bot that runs in GitHub Actions, built to replace
`pw-ci <https://github.com/ovsrobot/pw-ci>`_ for projects like Open
vSwitch, OVN and DPDK.

Every 15 minutes, a scheduled workflow runs ``leadtest``, which:

1. finds series that were completely received by Patchwork;
2. applies them on the right base branch and pushes them, patch by
   patch, to ``series_<id>`` branches of a test fork, which runs the
   project's workflows, and starts additional workflows (for example
   checkpatch) from the fork's ``leadtestkit`` branch;
3. posts ``pending`` checks to Patchwork and sends pending and final
   reports to the reports list (``ovs-build``, ``test-report``) in the
   format the existing tools understand;
4. reads the reports list archive and posts every report, including
   those of other labs, as Patchwork checks;
5. reruns workflows on ``Recheck-request:`` comments;
6. publishes the status of every tested series on GitHub Pages.

There is no server and no database: the state is a set of JSON files
on the ``state`` branch of the bot repository, committed after every
change, so its history can be reviewed and edited with git.

The bot is designed for unreliable servers.  Every request is retried,
every failed operation is retried in later runs with a backoff, and
anything that keeps failing is reported in the job summary and on the
pages instead of being dropped.  Patches are pushed with admission
control and per-author fairness, so one large series does not block
everyone else.

See `SETUP.rst <SETUP.rst>`_ to run an instance.


Development
-----------

::

  make          # tidy, audit, test and build
  make test
  make build    # ./leadtest

Dependencies are vendored.  Tests never contact real servers.

``leadtest run --state <dir> --once --dry-run --no-commit`` runs a
single read-only step against the real services configured in
``<dir>/config.yaml``.

Parts of the Patchwork client are derived from Leadlight, a Patchwork
TUI client; see ``NOTICE``.


License
-------

Apache License 2.0, see ``LICENSE``.
