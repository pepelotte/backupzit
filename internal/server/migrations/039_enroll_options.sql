-- Enrollment codes can be single-use and carry a label (e.g. a reference
-- of the management tool that asked for them); an agent keeps the label
-- of the code it enrolled with.
ALTER TABLE enrollment_tokens ADD COLUMN single_use BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE enrollment_tokens ADD COLUMN label TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN enroll_label TEXT NOT NULL DEFAULT '';
