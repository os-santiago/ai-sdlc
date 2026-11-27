# Try it: deliver one issue and watch it (TL;DR)

You need an installation with delivery enabled for your repository and the flow
console. The [detailed guide](try-it-detailed.md) explains each step.

1. **Install** with delivery and the console (once per repository; see the
   [adoption guide](adoption.md) for the manifest and model endpoint):

   ```bash
   ./install.sh local --cluster my-eval --repository OWNER/REPO \
     --environment ai-sdlc-production --profile verification \
     --independent-review --publication --merge --review-repair \
     --github-auth gh-cli --codex-auth-file ~/.codex/auth.json \
     --model-upstream upstream.json --project-manifest manifest.json \
     --flow-console
   ```

2. **Open the console:**

   ```bash
   kubectl -n ai-sdlc-runtime port-forward svc/ai-sdlc-flow-console 8080:8091
   ```

   Then browse to <http://127.0.0.1:8080/>.

3. **File a small issue** in that repository, with an `## Acceptance criteria`
   section, inside a package that already exists:

   ```markdown
   Report odd numbers.

   ## Acceptance criteria

   - `is_odd(n: int) -> bool` reports whether n is odd.
   - Tests cover 0, a negative odd number and a positive even number.
   ```

4. **Add the label `ai-sdlc:shadow`** to the issue, as the GitHub account the
   installation authorized. Creating the issue alone does nothing.

5. **Watch it** in the console: type the issue number in the "Issue" filter.
   In about 4 to 6 minutes (with a hosted model) the row goes through
   generation, verification, review, publication and merge. A pull request
   opens and merges, and the issue closes.

If it stops, the console's "What is wrong now" box names the stage and reason.
To retry, remove the label, wait about 30 seconds and add it again. Common
stops and fixes are in the [detailed guide](try-it-detailed.md#when-it-stops).
