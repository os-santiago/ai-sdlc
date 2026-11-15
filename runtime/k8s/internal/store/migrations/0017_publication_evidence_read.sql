-- Read-only policies; no operational grants or runtime activation.
CREATE POLICY publication_reader ON execution_requests FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON execution_lifecycles FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON execution_models FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON execution_candidates FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON model_allocations FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON model_budgets FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON work_items FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON plans FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON intake_heads FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON intake_revisions FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON intake_cursors FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON intake_observations FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON admission_heads FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON planning_heads FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_reader ON admissions FOR SELECT USING(current_user='aisdlc_release_manager');
