-- Integration test fixture for mysql-relation-deleter.
--
-- Runs unchanged on MySQL 5.7 and 8.0: no CTEs, window functions,
-- utf8mb4_0900_* collations, functional indexes or invisible columns.
-- Every table is InnoDB with an explicit utf8mb4_unicode_ci collation.
--
-- This file does not select a database. The Docker entrypoint loads it into
-- MYSQL_DATABASE (app), and mysqltest.ResetFixture runs USE before loading it.
--
-- Intended deletion root: users.id = 1 (alice).
--
-- Alice's closure (deleted with her): every row that references alice or a
-- row in her closure, through any FK or the manual relation in
-- testdata/relations.sample.yaml. Some of these rows belong to bob but are
-- reachable only through a cycle edge:
--   folders.id = 13       (bob's folder under alice's tree; via parent_id only)
--   team_members.id = 12  (bob in alice's team; via team_members.team_id only)
--   teams.id = 3          (bob's team led by alice's membership; via
--                          teams.lead_member_id only)
--   team_members.id = 31  (bob in team 3; via team 3 only)
--   audit_logs for order 101 written by bob (via audit_logs.order_id only)
-- audit_logs rows with user_id = 1 and order_id = 101/102 match through both
-- of the table's FKs and must be counted once.
--
-- Survivors (j): users.id = 2 (bob) and the rows that hang only off bob:
-- orders 201, order_items 2001, shipments (201,1), shipment_events 4,
-- folders 20 and 21, teams 2, team_members 21, audit_logs with user_id = 2
-- and order_id NULL or 201, newsletter_subscriptions 3,
-- subscription_deliveries 3, legacy_orders 3 and 4 (4 has customer_id NULL),
-- bob's device and token, files 2 and file_shares 2.

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (a) Parent/child FK chain, 3 levels: users -> orders -> order_items
-- (f) users.email is a UNIQUE non-PK key referenced by newsletter_subscriptions
-- ---------------------------------------------------------------------------
CREATE TABLE users (
  id BIGINT NOT NULL,
  email VARCHAR(191) NOT NULL,
  name VARCHAR(100) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_users_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE orders (
  id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  ordered_at DATETIME NOT NULL,
  PRIMARY KEY (id),
  KEY idx_orders_user (user_id),
  CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE order_items (
  id BIGINT NOT NULL,
  order_id BIGINT NOT NULL,
  sku VARCHAR(32) NOT NULL,
  quantity INT NOT NULL,
  price DECIMAL(10,2) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_order_items_order (order_id),
  CONSTRAINT fk_order_items_order FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (b) Composite PK (order_id, seq) and a composite FK that references it.
-- ---------------------------------------------------------------------------
CREATE TABLE shipments (
  order_id BIGINT NOT NULL,
  seq INT NOT NULL,
  carrier VARCHAR(32) NOT NULL,
  PRIMARY KEY (order_id, seq),
  CONSTRAINT fk_shipments_order FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE NO ACTION
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE shipment_events (
  id BIGINT NOT NULL,
  order_id BIGINT NOT NULL,
  shipment_seq INT NOT NULL,
  status VARCHAR(32) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_shipment_events_shipment (order_id, shipment_seq),
  CONSTRAINT fk_shipment_events_shipment FOREIGN KEY (order_id, shipment_seq)
    REFERENCES shipments (order_id, seq) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (c) Self-referencing tree: folders.parent_id -> folders.id
-- ---------------------------------------------------------------------------
CREATE TABLE folders (
  id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  parent_id BIGINT NULL,
  name VARCHAR(100) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_folders_user (user_id),
  KEY idx_folders_parent (parent_id),
  CONSTRAINT fk_folders_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_folders_parent FOREIGN KEY (parent_id) REFERENCES folders (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (d) Mutual references: teams.lead_member_id -> team_members.id and
--     team_members.team_id -> teams.id. Both are nullable so rows can be
--     inserted first and linked afterwards.
-- ---------------------------------------------------------------------------
CREATE TABLE teams (
  id BIGINT NOT NULL,
  owner_user_id BIGINT NOT NULL,
  lead_member_id BIGINT NULL,
  name VARCHAR(100) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_teams_owner (owner_user_id),
  KEY idx_teams_lead (lead_member_id),
  CONSTRAINT fk_teams_owner FOREIGN KEY (owner_user_id) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE team_members (
  id BIGINT NOT NULL,
  team_id BIGINT NULL,
  user_id BIGINT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_team_members_team (team_id),
  KEY idx_team_members_user (user_id),
  CONSTRAINT fk_team_members_team FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE SET NULL,
  CONSTRAINT fk_team_members_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE teams
  ADD CONSTRAINT fk_teams_lead_member FOREIGN KEY (lead_member_id)
    REFERENCES team_members (id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- (e) Child table without a primary key (and without any unique key). It has
--     two FKs, to users and to orders, so a row can be reached through both.
-- ---------------------------------------------------------------------------
CREATE TABLE audit_logs (
  user_id BIGINT NOT NULL,
  order_id BIGINT NULL,
  action VARCHAR(32) NOT NULL,
  logged_at DATETIME NOT NULL,
  KEY idx_audit_logs_user (user_id),
  KEY idx_audit_logs_order (order_id),
  CONSTRAINT fk_audit_logs_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT fk_audit_logs_order FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (f) FK referencing the UNIQUE non-PK column users.email, and a child of
--     that child (subscription_deliveries).
-- ---------------------------------------------------------------------------
CREATE TABLE newsletter_subscriptions (
  id BIGINT NOT NULL,
  email VARCHAR(191) NOT NULL,
  topic VARCHAR(32) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_newsletter_subscriptions_email (email),
  CONSTRAINT fk_newsletter_subscriptions_email FOREIGN KEY (email)
    REFERENCES users (email) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE subscription_deliveries (
  id BIGINT NOT NULL,
  subscription_id BIGINT NOT NULL,
  delivered_at DATETIME NOT NULL,
  PRIMARY KEY (id),
  KEY idx_subscription_deliveries_subscription (subscription_id),
  CONSTRAINT fk_subscription_deliveries_subscription FOREIGN KEY (subscription_id)
    REFERENCES newsletter_subscriptions (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (g) Relation without an FK constraint: legacy_orders.customer_id holds a
--     users.id. testdata/relations.sample.yaml defines it manually.
-- ---------------------------------------------------------------------------
CREATE TABLE legacy_orders (
  id BIGINT NOT NULL,
  customer_id BIGINT NULL,
  note VARCHAR(100) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_legacy_orders_customer (customer_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (h) Binary keys.
--     devices: BINARY(16) primary key, referenced by device_tokens.
--     files: VARBINARY(32) UNIQUE non-PK key (plus a BLOB column), referenced
--     by file_shares.
-- ---------------------------------------------------------------------------
CREATE TABLE devices (
  device_uuid BINARY(16) NOT NULL,
  user_id BIGINT NOT NULL,
  label VARCHAR(100) NOT NULL,
  PRIMARY KEY (device_uuid),
  KEY idx_devices_user (user_id),
  CONSTRAINT fk_devices_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE device_tokens (
  id BIGINT NOT NULL,
  device_uuid BINARY(16) NOT NULL,
  token VARBINARY(64) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_device_tokens_device (device_uuid),
  CONSTRAINT fk_device_tokens_device FOREIGN KEY (device_uuid)
    REFERENCES devices (device_uuid) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE files (
  id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  content_hash VARBINARY(32) NOT NULL,
  body BLOB NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_files_content_hash (content_hash),
  KEY idx_files_user (user_id),
  CONSTRAINT fk_files_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE file_shares (
  id BIGINT NOT NULL,
  content_hash VARBINARY(32) NOT NULL,
  shared_with VARCHAR(191) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_file_shares_content_hash (content_hash),
  CONSTRAINT fk_file_shares_content_hash FOREIGN KEY (content_hash)
    REFERENCES files (content_hash) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- (i) A view. The schema dump must exclude it.
-- ---------------------------------------------------------------------------
CREATE VIEW user_order_totals AS
  SELECT o.user_id AS user_id, COUNT(DISTINCT o.id) AS order_count,
         COALESCE(SUM(i.quantity * i.price), 0) AS total
  FROM orders o LEFT JOIN order_items i ON i.order_id = o.id
  GROUP BY o.user_id;

-- ---------------------------------------------------------------------------
-- Data. Rows owned by users.id = 1 (alice) form the deletion closure.
-- (j) Rows reachable only from users.id = 2 (bob) must survive deleting alice
--     (see the closure / survivor lists in the header).
-- ---------------------------------------------------------------------------
INSERT INTO users (id, email, name) VALUES
  (1, 'alice@example.com', 'alice'),
  (2, 'bob@example.com', 'bob');

INSERT INTO orders (id, user_id, ordered_at) VALUES
  (101, 1, '2024-01-01 10:00:00'),
  (102, 1, '2024-01-02 10:00:00'),
  (201, 2, '2024-01-03 10:00:00');

INSERT INTO order_items (id, order_id, sku, quantity, price) VALUES
  (1001, 101, 'SKU-A', 1, 10.00),
  (1002, 101, 'SKU-B', 2, 5.50),
  (1003, 102, 'SKU-A', 1, 10.00),
  (2001, 201, 'SKU-C', 3, 7.25);

-- (b)
INSERT INTO shipments (order_id, seq, carrier) VALUES
  (101, 1, 'yamato'),
  (101, 2, 'sagawa'),
  (201, 1, 'yamato');

INSERT INTO shipment_events (id, order_id, shipment_seq, status) VALUES
  (1, 101, 1, 'shipped'),
  (2, 101, 1, 'delivered'),
  (3, 101, 2, 'shipped'),
  (4, 201, 1, 'shipped');

-- (c) alice: 10 > 11 > 12 > 13, bob: 20 > 21.
-- Folder 13 belongs to bob but sits under alice's folder 12, so it is reached
-- only through parent_id and is in alice's closure.
INSERT INTO folders (id, user_id, parent_id, name) VALUES
  (10, 1, NULL, 'alice-root'),
  (11, 1, 10, 'alice-docs'),
  (12, 1, 11, 'alice-docs-2024'),
  (13, 2, 12, 'bob-shared-in-alice-docs'),
  (20, 2, NULL, 'bob-root'),
  (21, 2, 20, 'bob-photos');

-- (d) insert unlinked, then close the cycles.
-- Team 1 (alice) <-> member 11 (alice). Member 12 is bob in team 1, reached
-- only through team_members.team_id. Team 3 is owned by bob but led by member
-- 11, so it is reached only through teams.lead_member_id, and member 31 (bob
-- in team 3) only through team 3. Team 2 <-> member 21 is bob's and survives.
INSERT INTO teams (id, owner_user_id, lead_member_id, name) VALUES
  (1, 1, NULL, 'alice-team'),
  (2, 2, NULL, 'bob-team'),
  (3, 2, NULL, 'bob-team-led-by-alice');

INSERT INTO team_members (id, team_id, user_id) VALUES
  (11, 1, 1),
  (12, 1, 2),
  (21, 2, 2),
  (31, 3, 2);

UPDATE teams SET lead_member_id = 11 WHERE id = 1;
UPDATE teams SET lead_member_id = 21 WHERE id = 2;
UPDATE teams SET lead_member_id = 11 WHERE id = 3;

-- (e) duplicate rows on purpose: the table has no key to tell them apart.
-- Rows with user_id = 1 and order_id 101/102 match through both FKs; the bob
-- row for order 101 matches only through order_id. Rows with user_id = 2 and
-- order_id NULL or 201 survive.
INSERT INTO audit_logs (user_id, order_id, action, logged_at) VALUES
  (1, NULL, 'login', '2024-01-01 09:00:00'),
  (1, NULL, 'login', '2024-01-01 09:00:00'),
  (1, NULL, 'logout', '2024-01-01 18:00:00'),
  (1, 101, 'order', '2024-01-01 10:00:00'),
  (1, 102, 'order', '2024-01-02 10:00:00'),
  (2, 101, 'support', '2024-01-05 12:00:00'),
  (2, NULL, 'login', '2024-01-02 09:00:00'),
  (2, 201, 'order', '2024-01-03 10:00:00');

-- (f)
INSERT INTO newsletter_subscriptions (id, email, topic) VALUES
  (1, 'alice@example.com', 'news'),
  (2, 'alice@example.com', 'sale'),
  (3, 'bob@example.com', 'news');

INSERT INTO subscription_deliveries (id, subscription_id, delivered_at) VALUES
  (1, 1, '2024-02-01 08:00:00'),
  (2, 2, '2024-02-01 08:00:00'),
  (3, 3, '2024-02-01 08:00:00');

-- (g) customer_id NULL is an orphan that no user owns and must survive
INSERT INTO legacy_orders (id, customer_id, note) VALUES
  (1, 1, 'alice legacy'),
  (2, 1, 'alice legacy 2'),
  (3, 2, 'bob legacy'),
  (4, NULL, 'unassigned');

-- (h)
INSERT INTO devices (device_uuid, user_id, label) VALUES
  (UNHEX('11111111111111111111111111111111'), 1, 'alice-phone'),
  (UNHEX('00ff00ff00ff00ff00ff00ff00ff00ff'), 1, 'alice-laptop'),
  (UNHEX('22222222222222222222222222222222'), 2, 'bob-phone');

INSERT INTO device_tokens (id, device_uuid, token) VALUES
  (1, UNHEX('11111111111111111111111111111111'), UNHEX('a1a1')),
  (2, UNHEX('00ff00ff00ff00ff00ff00ff00ff00ff'), UNHEX('a2a2')),
  (3, UNHEX('22222222222222222222222222222222'), UNHEX('b1b1'));

INSERT INTO files (id, user_id, content_hash, body) VALUES
  (1, 1, UNHEX('aa01'), UNHEX('00010203')),
  (2, 2, UNHEX('bb01'), UNHEX('04050607'));

INSERT INTO file_shares (id, content_hash, shared_with) VALUES
  (1, UNHEX('aa01'), 'bob@example.com'),
  (2, UNHEX('bb01'), 'alice@example.com');
