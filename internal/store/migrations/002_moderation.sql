-- Moderation log: when and why a statement was hidden from the API.
alter table statements add column hidden_at timestamptz;
