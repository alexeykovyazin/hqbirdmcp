-- Sample migration project v1 (C.1 examples; spike3 dev target).
-- Up: the projects table plus a seed row.
CREATE TABLE FBMCP_MIG_PROJECTS (
	ID INTEGER NOT NULL PRIMARY KEY,
	NAME VARCHAR(80) NOT NULL,
	CREATED_AT TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO FBMCP_MIG_PROJECTS (ID, NAME) VALUES (1, 'first project');

-- @down
DROP TABLE FBMCP_MIG_PROJECTS;
