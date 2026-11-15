-- Independent read-only candidate views. No operational grants are added here.
CREATE POLICY verification_request_reader ON execution_requests FOR SELECT USING (current_user='aisdlc_verifier');
CREATE POLICY verification_lifecycle_reader ON execution_lifecycles FOR SELECT USING (current_user='aisdlc_verifier');
CREATE POLICY verification_model_reader ON execution_models FOR SELECT USING (current_user='aisdlc_verifier');
CREATE POLICY verification_candidate_reader ON execution_candidates FOR SELECT USING (current_user='aisdlc_verifier');
CREATE POLICY verification_allocation_reader ON model_allocations FOR SELECT USING (current_user='aisdlc_verifier');
CREATE POLICY verification_budget_reader ON model_budgets FOR SELECT USING (current_user='aisdlc_verifier');
