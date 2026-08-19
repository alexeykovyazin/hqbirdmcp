package fbparse

// OpKey is the canonical classification key consumed by the server-side
// v3 mapping (CI drift-gated against KnownOpKeys, §7).
type OpKey struct {
	Verb       Verb
	ObjectType ObjectType
	// Variant is the flag-reduced variant, e.g. "COLUMN_TYPE",
	// "GRANT_WITH_OPTION", "".
	Variant string
}

// Variant constants shared by the classifier and KnownOpKeys so neither
// side can drift.
const (
	varNone = ""

	varOrInsert  = "OR_INSERT" // UPDATE OR INSERT
	varWithLock  = "WITH_LOCK" // SELECT/UPDATE ... WITH LOCK
	varMergeSubQ = "USING_SUBQUERY"

	varIndexUnique         = "INDEX_UNIQUE"
	varIndexDesc           = "INDEX_DESC"
	varIndexUniqueDesc     = "INDEX_UNIQUE_DESC"
	varIndexExpression     = "INDEX_EXPRESSION"
	varIndexActive         = "INDEX_ACTIVE"
	varIndexInactive       = "INDEX_INACTIVE"
	varIndexPartial        = "INDEX_PARTIAL"
	varIndexValidateUnique = "INDEX_VALIDATE_UNIQUE" // P7.3: ALTER INDEX ... VALIDATE UNIQUE (FB5)

	// P7.4 materialized views (HQBird/FB5): view<->MV conversion forms.
	varViewToMaterialized    = "TO_MATERIALIZED"
	varViewToNotMaterialized = "TO_NOT_MATERIALIZED"

	varPackageBody      = "BODY"
	varFunctionAggr     = "AGGREGATE"
	varFunctionExternal = "EXTERNAL"

	varColumnAdd    = "COLUMN_ADD"
	varColumnDrop   = "COLUMN_DROP"
	varColumnRename = "COLUMN_RENAME"
	varColumnType   = "COLUMN_TYPE"
	varColumnNull   = "COLUMN_NOT_NULL"
	varColumnDef    = "COLUMN_DEFAULT"

	varConstraintPK    = "CONSTRAINT_PK"
	varConstraintUniq  = "CONSTRAINT_UNIQUE"
	varConstraintFK    = "CONSTRAINT_FK"
	varConstraintCheck = "CONSTRAINT_CHECK"
	varConstraintDrop  = "CONSTRAINT_DROP"

	varGrantWithOption   = "GRANT_WITH_OPTION"
	varGrantAdminOption  = "GRANT_ADMIN_OPTION"
	varRevokeGrantOption = "REVOKE_GRANT_OPTION"
	varRevokeAdminOption = "REVOKE_ADMIN_OPTION"
	varDCLClass          = "DDL" // class/database DDL privileges, no named object

	varSetGenerator   = "GENERATOR"
	varSetStatistics  = "STATISTICS"
	varSetTransaction = "TRANSACTION"
	varSetSession     = "SESSION"
	varSetTerm        = "TERM"

	varCurrentUser = "CURRENT_USER"
	varParameter   = "PARAMETER"
	varConstant    = "CONSTANT"
	varColumnSub   = "COLUMN"
)

// KnownOpKeys enumerates every OpKey the library can emit. The fbmcp CI
// drift gate cross-checks this against the generated v3 mapping so neither
// side can grow an unmapped or orphan classification (§7).
func KnownOpKeys() []OpKey {
	keys := make([]OpKey, 0, 96)

	// Reads (FR-7): SELECT and WITH..SELECT; WITH LOCK is a preview-warning
	// variant of the read verb.
	keys = append(keys,
		OpKey{VerbSelect, "", ""},
		OpKey{VerbSelect, "", varWithLock},
	)

	// DML (FR-9).
	keys = append(keys,
		OpKey{VerbInsert, ObjTable, ""},
		OpKey{VerbUpdate, ObjTable, ""},
		OpKey{VerbUpdate, ObjTable, varOrInsert},
		OpKey{VerbUpdate, ObjTable, varWithLock},
		OpKey{VerbDelete, ObjTable, ""},
		OpKey{VerbMerge, ObjTable, ""},
		OpKey{VerbMerge, ObjTable, varMergeSubQ},
	)

	// Execution (FR-2): EXECUTE BLOCK is always mutating.
	keys = append(keys,
		OpKey{VerbExecuteProc, ObjProcedure, ""},
		OpKey{VerbExecuteBlock, "", ""},
	)

	// Object vocabulary shared by the CREATE-family verbs (coverage per
	// parse.y create_clause / recreate_clause / replace_clause — the
	// classifier does not police verb×object validity, which is the
	// engine's job, so the family is uniform).
	for _, v := range []Verb{VerbCreate, VerbRecreate, VerbCreateOrAlter, VerbDrop, VerbAlter} {
		for _, ot := range []ObjectType{
			ObjTable, ObjGlobalTempTable, ObjExternalTable, ObjView,
			ObjProcedure, ObjFunction, ObjPackage, ObjTrigger,
			ObjSequence, ObjDomain, ObjException, ObjIndex,
			ObjUser, ObjRole, ObjMapping, ObjDatabase,
			ObjFilter, ObjShadow,
		} {
			keys = append(keys, OpKey{v, ot, ""})
		}
	}

	// Verb-specific shape variants.
	keys = append(keys,
		OpKey{VerbCreate, ObjIndex, varIndexUnique},
		OpKey{VerbCreate, ObjIndex, varIndexDesc},
		OpKey{VerbCreate, ObjIndex, varIndexUniqueDesc},
		OpKey{VerbCreate, ObjIndex, varIndexExpression},
		OpKey{VerbCreate, ObjIndex, varIndexPartial},
		OpKey{VerbAlter, ObjIndex, varIndexActive},
		OpKey{VerbAlter, ObjIndex, varIndexInactive},
		OpKey{VerbAlter, ObjIndex, varIndexValidateUnique},
		// P7.4 materialized views (HQBird/FB5). No CREATE OR ALTER form
		// (not documented) and no DROP form (DROP VIEW covers both kinds).
		OpKey{VerbCreate, ObjMaterializedView, ""},
		OpKey{VerbRecreate, ObjMaterializedView, ""},
		OpKey{VerbAlter, ObjMaterializedView, ""},
		OpKey{VerbAlter, ObjMaterializedView, varViewToNotMaterialized},
		OpKey{VerbAlter, ObjView, varViewToMaterialized},
		OpKey{VerbRefresh, ObjMaterializedView, ""},
		OpKey{VerbCreate, ObjPackage, varPackageBody},
		OpKey{VerbCreateOrAlter, ObjPackage, varPackageBody},
		OpKey{VerbRecreate, ObjPackage, varPackageBody},
		OpKey{VerbAlter, ObjPackage, varPackageBody},
		OpKey{VerbDrop, ObjPackage, varPackageBody},
		OpKey{VerbCreate, ObjFunction, varFunctionAggr},
		OpKey{VerbCreateOrAlter, ObjFunction, varFunctionAggr},
		OpKey{VerbRecreate, ObjFunction, varFunctionAggr},
		OpKey{VerbAlter, ObjFunction, varFunctionExternal},
		OpKey{VerbDrop, ObjFunction, varFunctionExternal},
	)

	// ALTER TABLE sub-operations (FR-5, FR-6).
	keys = append(keys,
		OpKey{VerbAlter, ObjTable, varColumnAdd},
		OpKey{VerbAlter, ObjTable, varColumnDrop},
		OpKey{VerbAlter, ObjTable, varColumnRename},
		OpKey{VerbAlter, ObjTable, varColumnType},
		OpKey{VerbAlter, ObjTable, varColumnNull},
		OpKey{VerbAlter, ObjTable, varColumnDef},
		OpKey{VerbAlter, ObjTable, varConstraintPK},
		OpKey{VerbAlter, ObjTable, varConstraintUniq},
		OpKey{VerbAlter, ObjTable, varConstraintFK},
		OpKey{VerbAlter, ObjTable, varConstraintCheck},
		OpKey{VerbAlter, ObjTable, varConstraintDrop},
	)

	// DCL (FR-4, FR-6).
	for _, v := range []Verb{VerbGrant, VerbRevoke} {
		for _, ot := range []ObjectType{
			ObjTable, ObjProcedure, ObjFunction, ObjPackage,
			ObjSequence, ObjException, ObjDomain, ObjDatabase,
			ObjRole, ObjFilter,
		} {
			keys = append(keys, OpKey{v, ot, ""})
			keys = append(keys, OpKey{v, ot, varDCLClass})
		}
	}
	keys = append(keys,
		OpKey{VerbGrant, ObjRole, varGrantAdminOption},
		OpKey{VerbGrant, ObjTable, varGrantWithOption},
		OpKey{VerbGrant, ObjProcedure, varGrantWithOption},
		OpKey{VerbGrant, ObjFunction, varGrantWithOption},
		OpKey{VerbGrant, ObjPackage, varGrantWithOption},
		OpKey{VerbGrant, ObjSequence, varGrantWithOption},
		OpKey{VerbGrant, ObjException, varGrantWithOption},
		OpKey{VerbGrant, ObjDomain, varGrantWithOption},
		OpKey{VerbGrant, ObjDatabase, varGrantWithOption},
		OpKey{VerbRevoke, ObjTable, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjProcedure, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjFunction, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjPackage, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjSequence, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjException, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjDomain, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjDatabase, varRevokeGrantOption},
		OpKey{VerbRevoke, ObjRole, varRevokeAdminOption},
	)

	// COMMENT ON (FR-5): container objects plus the column/parameter/
	// constant sub-object forms.
	for _, ot := range []ObjectType{
		ObjDatabase, ObjDomain, ObjTable, ObjView, ObjTrigger,
		ObjException, ObjSequence, ObjIndex, ObjPackage,
		ObjFilter, ObjRole, ObjProcedure, ObjFunction, ObjUser, ObjMapping,
	} {
		keys = append(keys, OpKey{VerbComment, ot, ""})
	}
	keys = append(keys,
		OpKey{VerbComment, ObjTable, varColumnSub},
		OpKey{VerbComment, ObjProcedure, varParameter},
		OpKey{VerbComment, ObjFunction, varParameter},
		OpKey{VerbComment, ObjPackage, varConstant},
	)

	// DECLARE (FR-4).
	keys = append(keys,
		OpKey{VerbDeclare, ObjFilter, ""},
		OpKey{VerbDeclare, ObjFunction, varFunctionExternal},
	)

	// SET (FR-16): objectless variants; SET GENERATOR carries the
	// sequence object.
	keys = append(keys,
		OpKey{VerbSet, ObjSequence, varSetGenerator},
		OpKey{VerbSet, ObjIndex, varSetStatistics},
		OpKey{VerbSet, "", varSetTransaction},
		OpKey{VerbSet, "", varSetSession},
		OpKey{VerbSet, "", varSetTerm},
	)

	// ALTER USER variants.
	keys = append(keys,
		OpKey{VerbAlter, ObjUser, varCurrentUser},
	)

	return keys
}
