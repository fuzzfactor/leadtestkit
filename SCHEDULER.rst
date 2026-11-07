Starting the bot from an external scheduler
===========================================

The bot repository's workflow (``.github/workflows/leadtest.yml``) runs
on a ``schedule``.  GitHub starts scheduled workflows late, sometimes by
an hour, and skips them when it is busy.  For steady reaction times,
start the workflow from a scheduler outside GitHub with a
``workflow_dispatch`` request:

.. code-block:: text

  POST https://api.github.com/repos/<owner>/<repo>/actions/workflows/leadtest.yml/dispatches
  Authorization: Bearer <token>
  Accept: application/vnd.github+json
  X-GitHub-Api-Version: 2022-11-28

  {"ref": "main"}

GitHub answers ``204 No Content`` when the run is queued.

Every 15 minutes matches the default ``runner.loop_duration`` of 13
minutes.  Runs never overlap: the workflow's ``concurrency`` group
keeps at most one run waiting while another one works, and a newer
waiting run replaces an older one, which then shows up as cancelled.
The built-in schedule can stay as a fallback.

This page shows three schedulers: a Cloudflare Worker, cron-job.org,
and a script run by cron on any machine.  Each needs credentials that
may start workflows in the bot repository, either a personal access
token or a GitHub App.  In all examples, replace ``<owner>/<repo>``
with the bot repository, for example ``ovs-ci/leadtestkit``.


Credentials
-----------

The credentials only need permission to start workflows in the bot
repository.  Do not reuse the App of the test fork (SETUP.rst, step 2)
here: it can push to repositories it is installed on, and the bot
repository holds the state branch.

Personal access token
~~~~~~~~~~~~~~~~~~~~~

Create a fine-grained token at
https://github.com/settings/personal-access-tokens/new:

- **Resource owner**: the account that owns the bot repository.  For
  an organization, its settings may require approving the token.
- **Expiration**: as long as allowed; the scheduler stops working when
  the token expires, so note the date.
- **Repository access**: *Only select repositories*, the bot
  repository.
- **Repository permissions**: Actions: read and write.

The token works in every scheduler below.  It acts as your account.

GitHub App
~~~~~~~~~~

An App has no expiration date and does not act as a person, but the
scheduler has to sign a JWT with the App's private key, which needs a
few lines of code.  cron-job.org cannot do that.

Create a second App, used only to start the bot, in the account that
owns the bot repository, as described in SETUP.rst, step 2, but with
only one repository permission:

- Actions: read and write (Metadata: read-only is added automatically).

Install it on the bot repository only, and note its Client ID (or App
ID) and private key.  The examples below find the installation from the repository
name and ask for a token limited to that repository and to Actions.


Cloudflare Worker
-----------------

Workers run on a schedule with cron triggers, which are included in
Cloudflare's free plan.  Everything can be done in the dashboard:

1. *Workers & Pages*, *Create*, start from the *Hello World* example,
   name it, for example ``leadtest-scheduler``, and *Deploy*.
2. *Edit code*, replace the code with one of the versions below and
   *Deploy*.
3. In the Worker's *Settings*, *Variables and Secrets*, add the
   variables listed with the code.  Use type *Secret* for credentials.
4. In *Settings*, *Trigger events*, add a *Cron trigger*, for example
   ``*/15 * * * *``.

To test it, use the *Schedule* button in the code editor's preview, or
wait for the trigger; the run appears in the bot repository's Actions
tab within seconds.  Errors appear in the Worker's logs and in the list
of cron events.

With a personal access token
~~~~~~~~~~~~~~~~~~~~~~~~~~~~

============  ======  ====================================
``REPO``      text    ``<owner>/<repo>``
``WORKFLOW``  text    ``leadtest.yml``
``REF``       text    ``main``
``TOKEN``     secret  the token
============  ======  ====================================

.. code-block:: js

  async function dispatch(env, token) {
    const url = `https://api.github.com/repos/${env.REPO}/actions/workflows/` +
      `${env.WORKFLOW}/dispatches`;
    const resp = await fetch(url, {
      method: "POST",
      headers: {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "leadtest-scheduler",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ ref: env.REF }),
    });
    if (!resp.ok) {
      throw new Error(`dispatch: HTTP ${resp.status}: ${await resp.text()}`);
    }
  }

  export default {
    async scheduled(event, env, ctx) {
      await dispatch(env, env.TOKEN);
      console.log(`Started ${env.WORKFLOW} on ${env.REPO}.`);
    },
  };

With a GitHub App
~~~~~~~~~~~~~~~~~

Workers can only import private keys in PKCS #8 form, while GitHub
gives them in PKCS #1 form (``BEGIN RSA PRIVATE KEY``).  Convert the
key once:

.. code-block:: sh

  openssl pkcs8 -topk8 -nocrypt -in app.private-key.pem -out app-pkcs8.pem

=====================  ======  ===================================
``REPO``               text    ``<owner>/<repo>``
``WORKFLOW``           text    ``leadtest.yml``
``REF``                text    ``main``
``APP_CLIENT_ID``      text    the Client ID (or App ID)
``APP_PRIVATE_KEY``    secret  the contents of ``app-pkcs8.pem``
=====================  ======  ===================================

.. code-block:: js

  const API = "https://api.github.com";
  const HEADERS = {
    "Accept": "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "leadtest-scheduler",
    "Content-Type": "application/json",
  };

  function b64url(data) {
    const bytes = typeof data === "string" ? new TextEncoder().encode(data)
                                           : new Uint8Array(data);
    let s = "";
    for (const b of bytes) s += String.fromCharCode(b);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  // A JWT authenticates as the App.  GitHub accepts at most ten
  // minutes; iat is backdated against clock skew.
  async function appJWT(env) {
    const pem = env.APP_PRIVATE_KEY.replace(/-----[^-]+-----/g, "").replace(/\s/g, "");
    const der = Uint8Array.from(atob(pem), c => c.charCodeAt(0));
    const key = await crypto.subtle.importKey("pkcs8", der,
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["sign"]);
    const now = Math.floor(Date.now() / 1000);
    const unsigned = b64url(JSON.stringify({ alg: "RS256", typ: "JWT" })) + "." +
      b64url(JSON.stringify({ iat: now - 60, exp: now + 540, iss: env.APP_CLIENT_ID }));
    const sig = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key,
      new TextEncoder().encode(unsigned));
    return unsigned + "." + b64url(sig);
  }

  async function github(method, path, token, body) {
    const resp = await fetch(API + path, {
      method,
      headers: { ...HEADERS, "Authorization": "Bearer " + token },
      body: body && JSON.stringify(body),
    });
    if (!resp.ok) {
      throw new Error(`${method} ${path}: HTTP ${resp.status}: ${await resp.text()}`);
    }
    return resp.status === 204 ? null : resp.json();
  }

  // Exchanges the JWT for a token limited to the bot repository and
  // to starting workflows.
  async function appToken(env) {
    const jwt = await appJWT(env);
    const inst = await github("GET", `/repos/${env.REPO}/installation`, jwt);
    const res = await github("POST", `/app/installations/${inst.id}/access_tokens`, jwt, {
      repositories: [env.REPO.split("/")[1]],
      permissions: { actions: "write" },
    });
    return res.token;
  }

  export default {
    async scheduled(event, env, ctx) {
      const token = await appToken(env);
      await github("POST", `/repos/${env.REPO}/actions/workflows/${env.WORKFLOW}/dispatches`,
        token, { ref: env.REF });
      console.log(`Started ${env.WORKFLOW} on ${env.REPO}.`);
    },
  };


cron-job.org
------------

https://cron-job.org is a free service that sends HTTP requests on a
schedule.  It cannot sign JWTs, so it needs a personal access token.

Create a cronjob with:

- **URL**: ``https://api.github.com/repos/<owner>/<repo>/actions/workflows/leadtest.yml/dispatches``
- **Execution schedule**: every 15 minutes.
- In the *Advanced* tab:

  - **Request method**: POST;
  - **Headers**:

    ========================  ================================
    ``Authorization``         ``Bearer <token>``
    ``Accept``                ``application/vnd.github+json``
    ``X-GitHub-Api-Version``  ``2022-11-28``
    ``Content-Type``          ``application/json``
    ========================  ================================

  - **Request body**: ``{"ref": "main"}``.

*Test run* shows GitHub's answer; ``204`` means the run was queued.
Enable failure notifications to learn when the token expires.


A script run by cron
--------------------

Any machine with ``cron``, ``curl`` and ``openssl`` can start the bot.
The App version also needs ``jq``.  Keep the credentials in a file only
the user running the script can read.

With a personal access token
~~~~~~~~~~~~~~~~~~~~~~~~~~~~

``/etc/leadtest-scheduler.conf``, mode ``600``:

.. code-block:: sh

  REPO=<owner>/<repo>
  WORKFLOW=leadtest.yml
  REF=main
  TOKEN=github_pat_...

``/usr/local/bin/leadtest-dispatch``:

.. code-block:: sh

  #!/bin/sh
  set -eu
  . /etc/leadtest-scheduler.conf

  curl -fsS -X POST \
      -H "Authorization: Bearer $TOKEN" \
      -H "Accept: application/vnd.github+json" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      -H "Content-Type: application/json" \
      -d "{\"ref\": \"$REF\"}" \
      "https://api.github.com/repos/$REPO/actions/workflows/$WORKFLOW/dispatches"
  echo "$(date -u +%FT%TZ) started $WORKFLOW on $REPO"

With a GitHub App
~~~~~~~~~~~~~~~~~

``openssl`` reads the private key as downloaded from GitHub.

``/etc/leadtest-scheduler.conf``, mode ``600``:

.. code-block:: sh

  REPO=<owner>/<repo>
  WORKFLOW=leadtest.yml
  REF=main
  APP_CLIENT_ID=Iv23li...
  KEY_FILE=/etc/leadtest-scheduler.pem

``/usr/local/bin/leadtest-dispatch``:

.. code-block:: sh

  #!/bin/sh
  set -eu
  . /etc/leadtest-scheduler.conf
  API=https://api.github.com

  b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

  # github TOKEN CURL-ARGUMENTS...
  github() {
      auth="Authorization: Bearer $1"
      shift
      curl -fsS -H "$auth" \
          -H "Accept: application/vnd.github+json" \
          -H "X-GitHub-Api-Version: 2022-11-28" \
          -H "Content-Type: application/json" "$@"
  }

  # A JWT authenticates as the App, for at most ten minutes.
  now=$(date +%s)
  header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
  claims=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' \
      $((now - 60)) $((now + 540)) "$APP_CLIENT_ID" | b64url)
  sig=$(printf '%s.%s' "$header" "$claims" |
      openssl dgst -sha256 -sign "$KEY_FILE" -binary | b64url)
  jwt="$header.$claims.$sig"

  # A token limited to the bot repository and to starting workflows.
  inst=$(github "$jwt" "$API/repos/$REPO/installation" | jq -r .id)
  token=$(github "$jwt" -X POST \
      -d "{\"repositories\": [\"${REPO#*/}\"], \"permissions\": {\"actions\": \"write\"}}" \
      "$API/app/installations/$inst/access_tokens" | jq -r .token)

  github "$token" -X POST -d "{\"ref\": \"$REF\"}" \
      "$API/repos/$REPO/actions/workflows/$WORKFLOW/dispatches"
  echo "$(date -u +%FT%TZ) started $WORKFLOW on $REPO"

Schedule
~~~~~~~~

Make the script executable and add it to the crontab of its user
(``crontab -e``):

.. code-block:: text

  */15 * * * * /usr/local/bin/leadtest-dispatch >> $HOME/leadtest-dispatch.log 2>&1

On machines with systemd, a timer does the same and logs to the
journal.  ``/etc/systemd/system/leadtest-dispatch.service``:

.. code-block:: ini

  [Unit]
  Description=Start the LeadTestKit workflow
  Wants=network-online.target
  After=network-online.target

  [Service]
  Type=oneshot
  ExecStart=/usr/local/bin/leadtest-dispatch

``/etc/systemd/system/leadtest-dispatch.timer``:

.. code-block:: ini

  [Unit]
  Description=Start the LeadTestKit workflow every 15 minutes

  [Timer]
  OnCalendar=*:0/15
  Persistent=true

  [Install]
  WantedBy=timers.target

Enable it with ``systemctl enable --now leadtest-dispatch.timer``.
