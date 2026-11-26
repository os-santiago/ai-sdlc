-- Read-only flow console observer (docs/operations/flow-console.md).
-- Each table below forces row security, so its existing per-role policies
-- would hide every row from a new role. These SELECT-only policies admit the
-- observer role to rows; they grant nothing by themselves. The role exists
-- only after role provisioning receives its optional credential, and then
-- holds column SELECT on timeline fields only (store.flowConsoleColumns): no
-- payload, receipt, proof, digest, token, delta or observation column.
-- No table, column, role, grant or row is added, so a configuration upgrade
-- may apply this file (see store.RelaxationMigrations). Tables without row
-- security (plans, work_items, admissions, model_calls) need only the grant.
CREATE POLICY flow_console_reader ON intake_observations FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_requests FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_lifecycles FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_effects FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_failures FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_candidates FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON execution_models FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON verification_requests FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON verification_effects FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON review_runs FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON review_effects FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON review_model_calls FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON publication_requests FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON publication_effects FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON published_records FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON merge_requests FOR SELECT USING (current_user='aisdlc_flow_console');
CREATE POLICY flow_console_reader ON merge_effects FOR SELECT USING (current_user='aisdlc_flow_console');
