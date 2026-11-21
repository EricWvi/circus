ALTER TABLE workspaces ADD COLUMN job_recorded_at INTEGER CHECK (job_recorded_at <> 0);

-- Legacy rows only have the workspace timestamp for the recorded job.
UPDATE workspaces SET job_recorded_at = updated_at WHERE job_id IS NOT NULL;
