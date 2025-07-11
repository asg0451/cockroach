// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"fmt"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlclustersettings"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

func qualifiedName(b BuildCtx, id catid.DescID) string {
	_, _, ns := scpb.FindNamespace(b.QueryByID(id))
	if ns == nil {
		// Function descriptors don't have namespace. So we need to handle this
		// special case here.
		return qualifiedFunctionName(b, id)
	}
	_, _, sc := scpb.FindNamespace(b.QueryByID(ns.SchemaID))
	_, _, db := scpb.FindNamespace(b.QueryByID(ns.DatabaseID))
	if db == nil {
		return ns.Name
	}
	if sc == nil {
		return db.Name + "." + ns.Name
	}
	return db.Name + "." + sc.Name + "." + ns.Name
}

func qualifiedFunctionName(b BuildCtx, id catid.DescID) string {
	elts := b.QueryByID(id)
	_, _, fnName := scpb.FindFunctionName(elts)
	_, _, objParent := scpb.FindSchemaChild(elts)
	_, _, scName := scpb.FindNamespace(b.QueryByID(objParent.SchemaID))
	_, _, scParent := scpb.FindSchemaParent(b.QueryByID(objParent.SchemaID))
	_, _, dbName := scpb.FindNamespace(b.QueryByID(scParent.ParentDatabaseID))
	return dbName.Name + "." + scName.Name + "." + fnName.Name
}

func simpleName(b BuildCtx, id catid.DescID) string {
	_, _, ns := scpb.FindNamespace(b.QueryByID(id))
	return ns.Name
}

// dropRestrictDescriptor contains the common logic for dropping something with
// RESTRICT.
func dropRestrictDescriptor(b BuildCtx, id catid.DescID) (hasChanged bool) {
	undropped := undroppedElements(b, id)
	if undropped.IsEmpty() {
		return false
	}
	undropped.ForEach(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) {
		if err := b.CheckPrivilege(e, privilege.DROP); err != nil {
			panic(err)
		}
		b.Drop(e)
	})
	return true
}

// undroppedElements returns the set of elements for a descriptor which need
// to be part of the target state of a DROP statement.
func undroppedElements(b BuildCtx, id catid.DescID) ElementResultSet {
	return b.QueryByID(id).Filter(func(current scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
		switch target {
		case scpb.InvalidTarget:
			// Validate that the descriptor-element is droppable, or already dropped.
			// This is the case when its current status is either PUBLIC or ABSENT,
			// which in the descriptor model correspond to it being in the PUBLIC state
			// or not being present at all.
			//
			// Objects undergoing an import or a backup restore will on the other hand
			// be have their descriptor states set to OFFLINE. When these descriptors
			// are decomposed to elements, these are then given scpb.InvalidTarget
			// target states by the decomposition logic.
			switch e.(type) {
			case *scpb.Database, *scpb.Schema, *scpb.Table, *scpb.Sequence, *scpb.View, *scpb.EnumType, *scpb.AliasType,
				*scpb.CompositeType:
				panic(errors.Wrapf(pgerror.Newf(pgcode.ObjectNotInPrerequisiteState,
					"object state is %s instead of PUBLIC, cannot be targeted by DROP", current),
					"%s", errMsgPrefix(b, id)))
			}
			// Ignore any other elements with undefined targets.
			return false
		case scpb.ToAbsent, scpb.TransientAbsent:
			// If the target is already ABSENT or TRANSIENT then the element is going
			// away anyway and so it doesn't need to have a target set for this DROP.
			return false
		}
		// Otherwise, return true to signal the removal of the element.
		return true
	})
}

// errMsgPrefix returns a human-readable prefix to scope error messages
// by the parent object's name and type. If the name can't be inferred we fall
// back on the descriptor ID.
func errMsgPrefix(b BuildCtx, id catid.DescID) string {
	typ := "descriptor"
	var name string
	b.QueryByID(id).ForEach(func(_ scpb.Status, target scpb.TargetStatus, e scpb.Element) {
		switch t := e.(type) {
		case *scpb.Database:
			typ = "database"
		case *scpb.Schema:
			typ = "schema"
		case *scpb.Table:
			typ = "table"
		case *scpb.Sequence:
			typ = "sequence"
		case *scpb.View:
			typ = "view"
		case *scpb.EnumType, *scpb.AliasType, *scpb.CompositeType:
			typ = "type"
		case *scpb.Namespace:
			// Set the name either from the first encountered Namespace element, or
			// if there are several (in case of a rename) from the one with the old
			// name.
			if name == "" || target == scpb.ToAbsent {
				name = t.Name
			}
		}
	})
	if name == "" {
		return fmt.Sprintf("%s #%d", typ, id)
	}
	return fmt.Sprintf("%s %q", typ, name)
}

// dropCascadeDescriptor contains the common logic for dropping something with
// CASCADE.
func dropCascadeDescriptor(b BuildCtx, id catid.DescID) {
	undropped := undroppedElements(b, id)
	// Exit early if all elements already have ABSENT targets.
	if undropped.IsEmpty() {
		return
	}
	// Check privileges and decide which actions to take or not.
	var isVirtualSchema bool
	undropped.ForEach(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) {
		switch t := e.(type) {
		case *scpb.Database:
			break
		case *scpb.Schema:
			if t.IsTemporary {
				panic(scerrors.NotImplementedErrorf(nil, "dropping a temporary schema"))
			}
			isVirtualSchema = t.IsVirtual
			// Return early to skip checking privileges on schemas.
			return
		case *scpb.Table:
			if t.IsTemporary {
				panic(scerrors.NotImplementedErrorf(nil, "dropping a temporary table"))
			}
		case *scpb.Sequence:
			if t.IsTemporary {
				panic(scerrors.NotImplementedErrorf(nil, "dropping a temporary sequence"))
			}
		case *scpb.View:
			if t.IsTemporary {
				panic(scerrors.NotImplementedErrorf(nil, "dropping a temporary view"))
			}
		case *scpb.EnumType, *scpb.AliasType, *scpb.CompositeType:
			break
		default:
			return
		}
		if err := b.CheckPrivilege(e, privilege.DROP); err != nil {
			panic(err)
		}
	})
	// Mark element targets as ABSENT.
	next := b.WithNewSourceElementID()
	undropped.ForEach(func(_ scpb.Status, target scpb.TargetStatus, e scpb.Element) {
		if isVirtualSchema {
			// Don't actually drop any elements of virtual schemas.
			return
		}
		b.Drop(e)
		switch t := e.(type) {
		case *scpb.EnumType:
			dropCascadeDescriptor(next, t.ArrayTypeID)
		case *scpb.CompositeType:
			dropCascadeDescriptor(next, t.ArrayTypeID)
		case *scpb.SequenceOwner:
			dropCascadeDescriptor(next, t.SequenceID)
		}
	})
	// Recurse on back-referenced elements.
	ub := undroppedBackrefs(b, id)
	ub.ForEach(func(_ scpb.Status, target scpb.TargetStatus, e scpb.Element) {
		switch t := e.(type) {
		case *scpb.SchemaParent:
			dropCascadeDescriptor(next, t.SchemaID)
		case *scpb.SchemaChild:
			dropCascadeDescriptor(next, t.ChildObjectID)
		case *scpb.View:
			dropCascadeDescriptor(next, t.ViewID)
		case *scpb.Sequence:
			dropCascadeDescriptor(next, t.SequenceID)
		case *scpb.AliasType:
			dropCascadeDescriptor(next, t.TypeID)
		case *scpb.EnumType:
			dropCascadeDescriptor(next, t.TypeID)
		case *scpb.CompositeType:
			dropCascadeDescriptor(next, t.TypeID)
		case *scpb.FunctionBody:
			dropCascadeDescriptor(next, t.FunctionID)
		case *scpb.TriggerFunctionCall:
			dropCascadeDescriptor(next, t.FuncID)
		case *scpb.TriggerDeps:
			dropCascadeDescriptor(next, t.TableID)
		case *scpb.PolicyDeps:
			dropCascadeDescriptor(next, t.TableID)
		case *scpb.Column, *scpb.ColumnType:
			// These only have type references.
			break
		case *scpb.Namespace, *scpb.Function, *scpb.SecondaryIndex, *scpb.PrimaryIndex,
			*scpb.TableLocalitySecondaryRegion, *scpb.Trigger:
			// These can be safely skipped and will be cleaned up on their own because
			// of dependents cleaned up above.
		case
			*scpb.ColumnDefaultExpression,
			*scpb.ColumnOnUpdateExpression,
			*scpb.ColumnComputeExpression,
			*scpb.PolicyUsingExpr,
			*scpb.PolicyWithCheckExpr,
			*scpb.CheckConstraint,
			*scpb.CheckConstraintUnvalidated,
			*scpb.ForeignKeyConstraint,
			*scpb.ForeignKeyConstraintUnvalidated,
			*scpb.SequenceOwner,
			*scpb.DatabaseRegionConfig:
			b.Drop(e)
		default:
			panic(errors.AssertionFailedf("un-dropped backref %T (%v) should either be "+
				"dropped or skipped", e, target))
		}
	})
}

func undroppedBackrefs(b BuildCtx, id catid.DescID) ElementResultSet {
	return b.BackReferences(id).Filter(func(_ scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
		return target == scpb.ToPublic && screl.ContainsDescID(e, id)
	})
}

func descIDs(input ElementResultSet) (ids catalog.DescriptorIDSet) {
	input.ForEach(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) {
		ids.Add(screl.GetDescID(e))
	})
	return ids
}

func columnElements(b BuildCtx, relationID catid.DescID, columnID catid.ColumnID) ElementResultSet {
	return b.QueryByID(relationID).Filter(func(
		current scpb.Status, target scpb.TargetStatus, e scpb.Element,
	) bool {
		idI, _ := screl.Schema.GetAttribute(screl.ColumnID, e)
		return idI != nil && idI.(catid.ColumnID) == columnID
	})
}

func constraintElements(
	b BuildCtx, relationID catid.DescID, constraintID catid.ConstraintID,
) ElementResultSet {
	return b.QueryByID(relationID).Filter(func(
		current scpb.Status, target scpb.TargetStatus, e scpb.Element,
	) bool {
		idI, _ := screl.Schema.GetAttribute(screl.ConstraintID, e)
		return idI != nil && idI.(catid.ConstraintID) == constraintID
	})
}

// getSortedColumnIDsInIndex return an all column IDs in an index, sorted.
func getSortedColumnIDsInIndex(
	b BuildCtx, tableID catid.DescID, indexID catid.IndexID,
) (ret []catid.ColumnID) {
	keys, keySuffixs, storeds := getSortedColumnIDsInIndexByKind(b, tableID, indexID)
	ret = append(keys, keySuffixs...)
	ret = append(ret, storeds...)
	return ret
}

// getSortedColumnIDsInIndexByKind return an index's key column IDs, key suffix
// column IDs, and storing column IDs, in sorted order.
func getSortedColumnIDsInIndexByKind(
	b BuildCtx, tableID catid.DescID, indexID catid.IndexID,
) (
	keyColumnIDs []catid.ColumnID,
	keySuffixColumnIDs []catid.ColumnID,
	storingColumnIDs []catid.ColumnID,
) {
	// Retrieve all columns of this index.
	allColumns := make([]*scpb.IndexColumn, 0)
	b.QueryByID(tableID).Filter(notFilter(ghostElementFilter)).FilterIndexColumn().
		ForEach(func(current scpb.Status, target scpb.TargetStatus, e *scpb.IndexColumn) {
			if e.IndexID != indexID {
				return
			}
			allColumns = append(allColumns, e)
		})

	// Sort all columns by their (Kind, OrdinalInKind).
	sort.Slice(allColumns, func(i, j int) bool {
		return (allColumns[i].Kind < allColumns[j].Kind) ||
			(allColumns[i].Kind == allColumns[j].Kind && allColumns[i].OrdinalInKind < allColumns[j].OrdinalInKind)
	})

	// Populate results.
	keyColumnIDs = make([]catid.ColumnID, 0)
	keySuffixColumnIDs = make([]catid.ColumnID, 0)
	storingColumnIDs = make([]catid.ColumnID, 0)
	for _, ice := range allColumns {
		switch ice.Kind {
		case scpb.IndexColumn_KEY:
			keyColumnIDs = append(keyColumnIDs, ice.ColumnID)
		case scpb.IndexColumn_KEY_SUFFIX:
			keySuffixColumnIDs = append(keySuffixColumnIDs, ice.ColumnID)
		case scpb.IndexColumn_STORED:
			storingColumnIDs = append(storingColumnIDs, ice.ColumnID)
		default:
			panic(fmt.Sprintf("Unknown index column element kind %v", ice.Kind))
		}
	}
	return keyColumnIDs, keySuffixColumnIDs, storingColumnIDs
}

// getNonDropColumnIDs returns all non-drop columns in a table.
func getNonDropColumns(b BuildCtx, tableID catid.DescID) (ret []*scpb.Column) {
	scpb.ForEachColumn(b.QueryByID(tableID).Filter(publicTargetFilter), func(
		_ scpb.Status, _ scpb.TargetStatus, e *scpb.Column,
	) {
		ret = append(ret, e)
	})
	return ret
}

// getColumnIDFromColumnName looks up a column's ID by its name.
// If no column with this name exists, 0 will be returned.
func getColumnIDFromColumnName(
	b BuilderState, tableID catid.DescID, columnName tree.Name, required bool,
) catid.ColumnID {
	colElems := b.ResolveColumn(tableID, columnName, ResolveParams{
		IsExistenceOptional: !required,
		RequiredPrivilege:   privilege.CREATE,
	})

	if colElems == nil {
		// no column with this name was found
		return 0
	}

	_, targetStatus, colElem := scpb.FindColumn(colElems)
	if colElem == nil {
		panic(errors.AssertionFailedf("programming error: cannot find a Column element for column %v", columnName))
	}
	if targetStatus == scpb.ToAbsent && required {
		panic(colinfo.NewUndefinedColumnError(string(columnName)))
	}
	return colElem.ColumnID
}

// mustGetColumnIDFromColumnName looks up a column's ID by its name.
// If no column with this name exists, panic.
func mustGetColumnIDFromColumnName(
	b BuildCtx, tableID catid.DescID, columnName tree.Name,
) catid.ColumnID {
	colID := getColumnIDFromColumnName(b, tableID, columnName, false)
	if colID == 0 {
		panic(errors.AssertionFailedf("programming erorr: cannot find column with name %v", columnName))
	}
	return colID
}

// Currently unused.
var _ = mustGetColumnIDFromColumnName

func mustGetTableIDFromTableName(b BuildCtx, tableName tree.TableName) catid.DescID {
	tableElems := b.ResolveTable(tableName.ToUnresolvedObjectName(), ResolveParams{
		IsExistenceOptional: false,
		RequiredPrivilege:   privilege.CREATE,
	})
	_, _, tableElem := scpb.FindTable(tableElems)
	if tableElem == nil {
		panic(errors.AssertionFailedf("programming error: cannot find a Table element for table %v", tableName))
	}
	return tableElem.TableID
}

func orFilter(
	fs ...func(scpb.Status, scpb.TargetStatus, scpb.Element) bool,
) func(scpb.Status, scpb.TargetStatus, scpb.Element) bool {
	return func(status scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
		ret := false
		for _, f := range fs {
			ret = ret || f(status, target, e)
		}
		return ret
	}
}

func notFilter(
	f func(scpb.Status, scpb.TargetStatus, scpb.Element) bool,
) func(scpb.Status, scpb.TargetStatus, scpb.Element) bool {
	return func(status scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
		return !f(status, target, e)
	}
}

func publicTargetFilter(_ scpb.Status, target scpb.TargetStatus, _ scpb.Element) bool {
	return target == scpb.ToPublic
}

func absentTargetFilter(_ scpb.Status, target scpb.TargetStatus, _ scpb.Element) bool {
	return target == scpb.ToAbsent
}

func transientTargetFilter(_ scpb.Status, target scpb.TargetStatus, _ scpb.Element) bool {
	return target == scpb.TransientAbsent
}

func validTargetFilter(_ scpb.Status, target scpb.TargetStatus, _ scpb.Element) bool {
	return target != scpb.InvalidTarget
}

func publicStatusFilter(status scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return status == scpb.Status_PUBLIC
}

func absentStatusFilter(status scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return status == scpb.Status_ABSENT
}

// "ghost elements" refer to those that were previously added (via b.Add or b.AddTransient)
// but later dropped (via b.Drop).
func ghostElementFilter(status scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
	return absentStatusFilter(status, target, e) && absentTargetFilter(status, target, e)
}

func notReachedTargetYetFilter(status scpb.Status, target scpb.TargetStatus, _ scpb.Element) bool {
	return status != target.Status()
}

func containsDescIDFilter(
	descID catid.DescID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		return screl.ContainsDescID(e, descID)
	}
}

func hasTableID(
	tableID catid.DescID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		return screl.GetDescID(e) == tableID
	}
}

func hasIndexIDAttrFilter(
	indexID catid.IndexID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		idI, _ := screl.Schema.GetAttribute(screl.IndexID, e)
		return idI != nil && idI.(catid.IndexID) == indexID
	}
}

func hasColumnIDAttrFilter(
	columnID catid.ColumnID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		idI, _ := screl.Schema.GetAttribute(screl.ColumnID, e)
		return idI != nil && idI.(catid.ColumnID) == columnID
	}
}

func hasConstraintIDAttrFilter(
	constraintID catid.ConstraintID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		idI, _ := screl.Schema.GetAttribute(screl.ConstraintID, e)
		return idI != nil && idI.(catid.ConstraintID) == constraintID
	}
}

func referencesColumnIDFilter(
	columnID catid.ColumnID,
) func(_ scpb.Status, _ scpb.TargetStatus, _ scpb.Element) bool {
	return func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) (included bool) {
		_ = screl.WalkColumnIDs(e, func(id *catid.ColumnID) error {
			if id != nil && *id == columnID {
				included = true
			}
			return nil
		})
		return included
	}
}

// indexColumnDirection converts tree.Direction to catenumpb.IndexColumn_Direction.
func indexColumnDirection(d tree.Direction) catenumpb.IndexColumn_Direction {
	switch d {
	case tree.DefaultDirection, tree.Ascending:
		return catenumpb.IndexColumn_ASC
	case tree.Descending:
		return catenumpb.IndexColumn_DESC
	default:
		panic(errors.AssertionFailedf("unknown direction %s", d))
	}
}

// indexSpec holds an index element and its children.
type indexSpec struct {
	primary   *scpb.PrimaryIndex
	secondary *scpb.SecondaryIndex
	temporary *scpb.TemporaryIndex

	name          *scpb.IndexName
	partitioning  *scpb.IndexPartitioning
	columns       []*scpb.IndexColumn
	idxComment    *scpb.IndexComment
	constrComment *scpb.ConstraintComment
	data          *scpb.IndexData
}

// indexSpecMutator holds an index spec designed for mutatioin
type indexSpecMutator struct {
	indexSpec
}

// applyDeltaForIndexColumns updates index column configurations by applying
// changes from a previous to the current state. It removes outdated elements
// and incorporates new or updated elements into the build context for processing.
func (s *indexSpecMutator) applyDeltaForIndexColumns(
	b BuildCtx, prev *indexSpec, isIndexFinal bool,
) {
	columnIDToElem := make(map[catid.ColumnID]*scpb.IndexColumn)
	// Remove all old elements.
	for _, col := range prev.columns {
		columnIDToElem[col.ColumnID] = col
		b.Drop(col)
	}
	// Next, apply the current state.
	for idx, col := range s.columns {
		// If the element already exists, we just need to copy
		// the update in there.
		if existingCol, ok := columnIDToElem[col.ColumnID]; ok {
			*existingCol = *protoutil.Clone(col).(*scpb.IndexColumn)
			col = existingCol
			s.columns[idx] = col
		}
		if isIndexFinal {
			b.Add(col)
		} else {
			b.AddTransient(col)
		}
	}
}

// apply makes it possible to conveniently define build targets for all
// the elements in the indexSpec.
func (s *indexSpec) apply(fn func(e scpb.Element)) {
	if s.primary != nil {
		fn(s.primary)
	}
	if s.secondary != nil {
		fn(s.secondary)
	}
	if s.temporary != nil {
		fn(s.temporary)
	}
	if s.name != nil {
		fn(s.name)
	}
	if s.partitioning != nil {
		fn(s.partitioning)
	}
	for _, ic := range s.columns {
		fn(ic)
	}
	if s.idxComment != nil {
		fn(s.idxComment)
	}
	if s.constrComment != nil {
		fn(s.constrComment)
	}
	if s.data != nil {
		fn(s.data)
	}
}

func (s *indexSpec) makeMutator() *indexSpecMutator {
	m := &indexSpecMutator{indexSpec: s.clone()}
	m.orderColumns()
	return m
}

// clone conveniently deep-copies all the elements in the indexSpec.
func (s *indexSpec) clone() (c indexSpec) {
	if s.primary != nil {
		c.primary = protoutil.Clone(s.primary).(*scpb.PrimaryIndex)
	}
	if s.secondary != nil {
		c.secondary = protoutil.Clone(s.secondary).(*scpb.SecondaryIndex)
	}
	if s.temporary != nil {
		c.temporary = protoutil.Clone(s.temporary).(*scpb.TemporaryIndex)
	}
	if s.name != nil {
		c.name = protoutil.Clone(s.name).(*scpb.IndexName)
	}
	if s.partitioning != nil {
		c.partitioning = protoutil.Clone(s.partitioning).(*scpb.IndexPartitioning)
	}
	for _, ic := range s.columns {
		c.columns = append(c.columns, protoutil.Clone(ic).(*scpb.IndexColumn))
	}
	if s.idxComment != nil {
		c.idxComment = protoutil.Clone(s.idxComment).(*scpb.IndexComment)
	}
	if s.constrComment != nil {
		c.constrComment = protoutil.Clone(s.constrComment).(*scpb.ConstraintComment)
	}
	if s.data != nil {
		c.data = protoutil.Clone(s.data).(*scpb.IndexData)
	}
	return c
}

func (s *indexSpec) tableID() catid.DescID {
	if s.primary != nil {
		return s.primary.TableID
	}
	if s.temporary != nil {
		return s.temporary.TableID
	}
	if s.secondary != nil {
		return s.secondary.TableID
	}
	return 0
}

// columnComparison compares two index columns based on their kind and ordinal
// position within the kind.
func (s *indexSpecMutator) columnComparison(i, j int) bool {
	return s.columns[i].Kind < s.columns[j].Kind &&
		s.columns[i].OrdinalInKind < s.columns[j].OrdinalInKind
}

// orderColumns the column field by kind and type.
func (s *indexSpecMutator) orderColumns() {
	sort.Slice(s.columns, s.columnComparison)
}

func (s *indexSpecMutator) reassignOrdinals(kind scpb.IndexColumn_Kind) {
	// Re-number all columns of this kind.
	numColKind := 0
	for _, col := range s.columns {
		if col.Kind == kind {
			col.OrdinalInKind = uint32(numColKind)
			numColKind++
		}
	}
}

// resetColumns clears all the columns in specifications.
func (s *indexSpecMutator) resetColumns() {
	s.columns = s.columns[:0]
}

// removeColumn deletes a column by ID and adjusts ordinals
// after.
func (s *indexSpecMutator) removeColumn(columnID catid.ColumnID, kind scpb.IndexColumn_Kind) {
	// The indexSpec must have ordered columns for this to work.
	if buildutil.CrdbTestBuild &&
		!sort.SliceIsSorted(s.columns, s.columnComparison) {
		panic(errors.AssertionFailedf("indexSpec was not sorted first"))
	}
	for i, col := range s.columns {
		if col.ColumnID == columnID && kind == col.Kind {
			kind = col.Kind
			s.columns = append(s.columns[:i], s.columns[i+1:]...)
			s.reassignOrdinals(kind)
			return
		}
	}
}

// removeImplicitColumns removes all implicit columns.
func (s *indexSpecMutator) removeImplicitColumns() {
	// The indexSpec must have ordered columns for this to work.
	if buildutil.CrdbTestBuild &&
		!sort.SliceIsSorted(s.columns, s.columnComparison) {
		panic(errors.AssertionFailedf("indexSpec was not sorted first"))
	}
	newColumns := make([]*scpb.IndexColumn, 0, len(s.columns))
	for _, col := range s.columns {
		if col.Implicit {
			continue
		}
		newColumns = append(newColumns, col)
	}
	s.columns = newColumns
}

// assertColumnIsNotContained validates the column doesn't already exist.
func (s *indexSpec) assertColumnIsNotContained(column *scpb.IndexColumn) {
	if !buildutil.CrdbTestBuild {
		return
	}
	for _, col := range s.columns {
		if col.ColumnID == column.ColumnID && col.Kind == column.Kind {
			panic(errors.AssertionFailedf("column %d %d %d already exists",
				column.ColumnID, column.Kind, column.OrdinalInKind))
		}
	}
}

// prependColumn columns before all others of the same kind. The columns should
// be sorted first.
func (s *indexSpecMutator) prependColumn(column *scpb.IndexColumn) {
	// Sanity: Validate the column is not already contained.
	s.assertColumnIsNotContained(column)
	// The indexSpec must have ordered columns for this to work.
	if buildutil.CrdbTestBuild &&
		!sort.SliceIsSorted(s.columns, s.columnComparison) {
		panic(errors.AssertionFailedf("indexSpec was not sorted first"))
	}

	// Determine the offset where we should add this column.
	insertionPoint := 0
	for idx, col := range s.columns {
		if col.Kind <= column.Kind {
			insertionPoint = idx
		}
		if col.Kind >= column.Kind {
			break
		}
	}
	s.columns = append(s.columns[:insertionPoint], append([]*scpb.IndexColumn{column}, s.columns[insertionPoint:]...)...)
	s.reassignOrdinals(column.Kind)
}

// appendColumn columns after all others of the same kind. The columns should
// be sorted first.
func (s *indexSpecMutator) appendColumn(column *scpb.IndexColumn) {
	// Sanity: Validate the column is not already contained.
	s.assertColumnIsNotContained(column)
	// The indexSpec must have ordered columns for this to work.
	if buildutil.CrdbTestBuild &&
		!sort.SliceIsSorted(s.columns, s.columnComparison) {
		panic(errors.AssertionFailedf("indexSpec was not sorted first"))
	}
	// Find the insertion point.
	insertionPoint := len(s.columns)
	for i, col := range s.columns {
		if col.Kind > column.Kind {
			insertionPoint = i
			break
		}
	}
	s.columns = append(s.columns[:insertionPoint], append([]*scpb.IndexColumn{column}, s.columns[insertionPoint:]...)...)
	s.reassignOrdinals(column.Kind)
}

func (s *indexSpec) indexID() catid.IndexID {
	if s.primary != nil {
		return s.primary.IndexID
	}
	if s.temporary != nil {
		return s.temporary.IndexID
	}
	if s.secondary != nil {
		return s.secondary.IndexID
	}
	return 0
}

func (s *indexSpec) SourceIndexID() catid.IndexID {
	if s.primary != nil {
		return s.primary.SourceIndexID
	}
	if s.temporary != nil {
		return s.temporary.SourceIndexID
	}
	if s.secondary != nil {
		return s.secondary.SourceIndexID
	}
	return 0
}

// makeIndexSpec constructs an indexSpec based on an existing index element.
func makeIndexSpec(b BuildCtx, tableID catid.DescID, indexID catid.IndexID) (s indexSpec) {
	tableElts := b.QueryByID(tableID)
	idxElts := tableElts.Filter(hasIndexIDAttrFilter(indexID)).Filter(validTargetFilter).Filter(notFilter(ghostElementFilter))
	var constraintID catid.ConstraintID
	var n int
	_, _, s.primary = scpb.FindPrimaryIndex(idxElts)
	if s.primary != nil {
		constraintID = s.primary.ConstraintID
		n++
	}
	_, _, s.secondary = scpb.FindSecondaryIndex(idxElts)
	if s.secondary != nil {
		constraintID = s.secondary.ConstraintID
		n++
	}
	_, _, s.temporary = scpb.FindTemporaryIndex(idxElts)
	if s.temporary != nil {
		constraintID = s.temporary.ConstraintID
		n++
	}
	if n != 1 {
		panic(errors.AssertionFailedf("invalid index spec for TableID=%d and IndexID=%d: "+
			"primary=%v, secondary=%v, temporary=%v",
			tableID, indexID, s.primary != nil, s.secondary != nil, s.temporary != nil))
	}
	_, _, s.name = scpb.FindIndexName(idxElts)
	_, _, s.partitioning = scpb.FindIndexPartitioning(idxElts)
	scpb.ForEachIndexColumn(idxElts, func(_ scpb.Status, _ scpb.TargetStatus, ic *scpb.IndexColumn) {
		s.columns = append(s.columns, ic)
	})
	_, _, s.idxComment = scpb.FindIndexComment(idxElts)
	scpb.ForEachConstraintComment(tableElts, func(_ scpb.Status, _ scpb.TargetStatus, cc *scpb.ConstraintComment) {
		if cc.ConstraintID == constraintID {
			s.constrComment = cc
		}
	})
	_, _, s.data = scpb.FindIndexData(idxElts)
	return s
}

// makeTempIndexSpec clones the primary/secondary index spec into one for a
// temporary index, based on the populated information.
func makeTempIndexSpec(src indexSpec) indexSpec {
	if src.secondary == nil && src.primary == nil {
		panic(errors.AssertionFailedf("make temp index converts a primary/secondary index into a temporary one"))
	}
	newTempSpec := src.clone()
	var srcIdx scpb.Index
	var expr *scpb.Expression
	isSecondary := false
	if src.primary != nil {
		srcIdx = newTempSpec.primary.Index
	}
	if src.secondary != nil {
		srcIdx = newTempSpec.secondary.Index
		expr = newTempSpec.secondary.EmbeddedExpr
		isSecondary = true
	}
	tempID := srcIdx.TemporaryIndexID
	newTempSpec.temporary = &scpb.TemporaryIndex{
		Index:                    srcIdx,
		IsUsingSecondaryEncoding: isSecondary,
		Expr:                     expr,
	}
	newTempSpec.temporary.TemporaryIndexID = 0
	newTempSpec.temporary.IndexID = tempID
	newTempSpec.temporary.ConstraintID = srcIdx.ConstraintID + 1
	newTempSpec.secondary = nil
	newTempSpec.primary = nil

	// Replace all the index IDs in the clone.
	if newTempSpec.data != nil {
		newTempSpec.data.IndexID = tempID
	}
	if newTempSpec.partitioning != nil {
		newTempSpec.partitioning.IndexID = tempID
	}
	for _, ic := range newTempSpec.columns {
		ic.IndexID = tempID
	}
	// Clear fields that temporary indexes should not have.
	newTempSpec.name = nil
	newTempSpec.constrComment = nil
	newTempSpec.idxComment = nil
	newTempSpec.name = nil
	newTempSpec.secondary = nil

	return newTempSpec
}

// indexColumnSpec specifies how to construct a scpb.IndexColumn element.
// Note it is table and index agnostic.
type indexColumnSpec struct {
	columnID     catid.ColumnID
	kind         scpb.IndexColumn_Kind
	direction    catenumpb.IndexColumn_Direction
	implicit     bool
	invertedKind catpb.InvertedIndexColumnKind
}

func makeIndexColumnSpec(ic *scpb.IndexColumn) indexColumnSpec {
	return indexColumnSpec{
		columnID:     ic.ColumnID,
		kind:         ic.Kind,
		direction:    ic.Direction,
		implicit:     ic.Implicit,
		invertedKind: ic.InvertedKind,
	}
}

// makeSwapIndexSpec constructs a pair of indexSpec for the new index and the
// accompanying temporary index to swap out an existing index with.
//
// `inUseTempIDs`, if true, assign "temporary"/"placeholder" index/constraint
// IDs for the to-be-returned index pair. Otherwise, actual, final index/constraint
// IDs will be assigned to them.
func makeSwapIndexSpec(
	b BuildCtx,
	out indexSpec,
	inSourceIndexID catid.IndexID,
	inColumns []indexColumnSpec,
	inUseTentativeIDs bool,
) (in, temp indexSpec) {
	isSecondary := out.secondary != nil
	// Determine table ID and validate input.
	var tableID catid.DescID
	{
		var n int
		var outID catid.IndexID
		if isSecondary {
			tableID = out.secondary.TableID
			outID = out.secondary.IndexID
			n++
		}
		if out.primary != nil {
			tableID = out.primary.TableID
			outID = out.primary.IndexID
			n++
		}
		if out.temporary != nil {
			tableID = out.primary.TableID
			outID = out.primary.IndexID
		}
		if n != 1 {
			panic(errors.AssertionFailedf("invalid swap source index spec for TableID=%d and IndexID=%d: "+
				"primary=%v, secondary=%v, temporary=%v",
				tableID, outID, out.primary != nil, isSecondary, out.temporary != nil))
		}
	}
	// Determine old and new IDs.
	var inID, inTempID catid.IndexID
	var inConstraintID catid.ConstraintID
	if inUseTentativeIDs {
		inID = b.NextTableTentativeIndexID(tableID)
		inTempID = inID + 1
		inConstraintID = b.NextTableTentativeConstraintID(tableID)
	} else {
		inID = b.NextTableIndexID(tableID)
		inTempID = inID + 1
		inConstraintID = b.NextTableConstraintID(tableID)
	}

	// Setup new primary or secondary index.
	{
		in = out.clone()
		var idx *scpb.Index
		if isSecondary {
			idx = &in.secondary.Index
		} else {
			idx = &in.primary.Index
		}
		idx.IndexID = inID
		idx.SourceIndexID = inSourceIndexID
		idx.TemporaryIndexID = inTempID
		idx.ConstraintID = inConstraintID
		if in.name != nil {
			in.name.IndexID = inID
		}
		if in.partitioning != nil {
			in.partitioning.IndexID = inID
		}
		m := make(map[scpb.IndexColumn_Kind]uint32)
		in.columns = in.columns[:0]
		for _, cs := range inColumns {
			ordinalInKind := m[cs.kind]
			m[cs.kind] = ordinalInKind + 1
			in.columns = append(in.columns, &scpb.IndexColumn{
				TableID:       idx.TableID,
				IndexID:       inID,
				ColumnID:      cs.columnID,
				OrdinalInKind: ordinalInKind,
				Kind:          cs.kind,
				Direction:     cs.direction,
				Implicit:      cs.implicit,
				InvertedKind:  cs.invertedKind,
			})
		}
		if in.idxComment != nil {
			in.idxComment.IndexID = inID
		}
		if in.constrComment != nil {
			in.constrComment.ConstraintID = inConstraintID
		}
		if in.data != nil {
			in.data.IndexID = inID
		}
	}
	// Setup temporary index.
	{
		temp = makeTempIndexSpec(in)
	}
	return in, temp
}

// ExtractColumnIDsInExpr extracts column IDs used in expr. It's similar to
// schemaexpr.ExtractColumnIDs but this function can also extract columns
// added in the same transaction (e.g. for `ADD COLUMN j INT CHECK (j > 0);`,
// schemaexpr.ExtractColumnIDs will err with "column j does not exist", but
// this function can successfully retrieve the ID of column j from the builder state).
func ExtractColumnIDsInExpr(
	b BuilderState, tableID catid.DescID, expr tree.Expr,
) (catalog.TableColSet, error) {
	var colIDs catalog.TableColSet

	_, err := tree.SimpleVisit(expr, func(expr tree.Expr) (recurse bool, newExpr tree.Expr, err error) {
		vBase, ok := expr.(tree.VarName)
		if !ok {
			return true, expr, nil
		}

		v, err := vBase.NormalizeVarName()
		if err != nil {
			return false, nil, err
		}

		c, ok := v.(*tree.ColumnItem)
		if !ok {
			return true, expr, nil
		}

		colID := getColumnIDFromColumnName(b, tableID, c.ColumnName, false /* required */)
		colIDs.Add(colID)
		return false, expr, nil
	})

	return colIDs, err
}

func isColNotNull(b BuildCtx, tableID catid.DescID, columnID catid.ColumnID) (ret bool) {
	// A column is NOT NULL iff there is a ColumnNotNull element on this columnID
	scpb.ForEachColumnNotNull(b.QueryByID(tableID), func(
		current scpb.Status, target scpb.TargetStatus, e *scpb.ColumnNotNull,
	) {
		if e.ColumnID == columnID {
			ret = true
		}
	})
	return ret
}

func maybeFailOnCrossDBTypeReference(b BuildCtx, typeID descpb.ID, parentDBID descpb.ID) {
	_, _, typeNamespace := scpb.FindNamespace(b.QueryByID(typeID))
	if typeNamespace.DatabaseID != parentDBID {
		typeName := tree.MakeTypeNameWithPrefix(b.NamePrefix(typeNamespace), typeNamespace.Name)
		panic(pgerror.Newf(
			pgcode.FeatureNotSupported,
			"cross database type references are not supported: %s",
			typeName.String()))
	}
}

// shouldSkipValidatingConstraint determines whether we should
// skip validating this constraint.
//
// We skip validating the constraint if it's already validated.
// We return non-nil error if the constraint is being dropped.
func shouldSkipValidatingConstraint(
	b BuildCtx, tableID catid.DescID, constraintID catid.ConstraintID,
) (skip bool, err error) {
	// Retrieve constraint and table name for potential error messages.
	constraintElems := constraintElements(b, tableID, constraintID)
	_, _, tableNameElem := scpb.FindNamespace(b.QueryByID(tableID))
	_, _, constraintNameElem := scpb.FindConstraintWithoutIndexName(constraintElems)

	constraintElems.ForEach(func(
		current scpb.Status, target scpb.TargetStatus, e scpb.Element,
	) {
		switch e.(type) {
		case *scpb.CheckConstraint, *scpb.UniqueWithoutIndexConstraint,
			*scpb.ForeignKeyConstraint:
			if current == scpb.Status_PUBLIC && target == scpb.ToPublic {
				skip = true
			} else if current == scpb.Status_ABSENT && target == scpb.ToPublic {
				err = pgerror.Newf(pgcode.ObjectNotInPrerequisiteState,
					"constraint %q in the middle of being added, try again later", constraintNameElem.Name)
			} else {
				err = sqlerrors.NewUndefinedConstraintError(constraintNameElem.Name, tableNameElem.Name)
			}
		case *scpb.CheckConstraintUnvalidated, *scpb.UniqueWithoutIndexConstraintUnvalidated,
			*scpb.ForeignKeyConstraintUnvalidated:
			if current == scpb.Status_PUBLIC && target == scpb.ToPublic {
				skip = false
			} else if current == scpb.Status_ABSENT && target == scpb.ToPublic {
				// TODO (xiang): Allow this by allowing VALIDATE CONSTRAINT to perform
				// validation in statement phase. This condition occurs when we do thing
				// like `ALTER TABLE .. ADD CONSTRAINT .. NOT VALID; VALIDATE CONSTRAINT ..`,
				// or, validating a constraint created in the same transaction earlier.
				err = scerrors.NotImplementedErrorf(nil, "validate constraint created in same txn")
			} else {
				err = sqlerrors.NewUndefinedConstraintError(constraintNameElem.Name, tableNameElem.Name)
			}
		}
	})
	return skip, err
}

// maybeCleanupSchemaLocked will clean up any schema_locked elements if the
// statement turns out to be idempotent.
func maybeCleanupSchemaLocked(b BuildCtx, id catid.DescID) {
	// We need to check all elements by this ID and any back references
	// to this element.
	elts := b.QueryByID(id)
	backRefElts := b.BackReferences(id)
	// Detect any non-schema locked elements for this table that are
	// being modified. If none exists, then the schema_locked element will
	// be added back, since it was made TRANSIENT_ABSENT inside
	// checkTableSchemaChangePrerequisites.
	modifiesNonSchemaLockedElements := func(elts ElementResultSet) bool {
		return !elts.Filter(notReachedTargetYetFilter).Filter(validTargetFilter).Filter(func(current scpb.Status, target scpb.TargetStatus, e scpb.Element) bool {
			switch e.(type) {
			case *scpb.TableSchemaLocked:
				return false
			default:
				return true
			}
		}).IsEmpty()
	}

	// This schema change was a no-op, so schema_locked doesn't matter.
	if !modifiesNonSchemaLockedElements(elts) && !modifiesNonSchemaLockedElements(backRefElts) {
		b.Add(elts.FilterTableSchemaLocked().MustGetOneElement())
	}
}

// checkTableSchemaChangePrerequisites checks any pre-requisites before a table
// schema change is allowed. This function panics if a schema change is not
// allowed on this table. A schema change is disallowed if one of the following
// is true:
//   - The table is referenced by logical data replication jobs, and the statement
//     is not in the allow list of LDR schema changes.
//   - schema_locked if the current version does not support transient drops
//     of the lock.
//
// If the table in question is schema_locked, this logic removes the schema_locked
// in a transient manner, allowing it to restore after the schema change.
func checkTableSchemaChangePrerequisites(
	b BuildCtx, tableElements ElementResultSet, n tree.Statement,
) (maybeCleanupSchemaLockedFn func()) {
	schemaLocked := tableElements.FilterTableSchemaLocked().MustGetZeroOrOneElement()
	// No-op by default unless schema_locked has been setup.
	maybeCleanupSchemaLockedFn = func() {}
	if schemaLocked != nil && !tree.IsSetOrResetSchemaLocked(n) {
		// Before 25.2 we don't support auto-unsetting schema locked.
		if !b.ClusterSettings().Version.IsActive(b, clusterversion.V25_2) {
			ns := tableElements.FilterNamespace().MustGetOneElement()
			panic(sqlerrors.NewSchemaChangeOnLockedTableErr(ns.Name))
		}
		// Unset schema_locked for the user.
		b.DropTransient(schemaLocked)
		maybeCleanupSchemaLockedFn = func() {
			maybeCleanupSchemaLocked(b, tableElements.FilterTable().MustGetOneElement().TableID)
		}
	}
	_, _, ldrJobIDs := scpb.FindLDRJobIDs(tableElements)
	if ldrJobIDs != nil && len(ldrJobIDs.JobIDs) > 0 {
		var virtualColNames []string
		scpb.ForEachColumnType(tableElements, func(current scpb.Status, target scpb.TargetStatus, colTypeElem *scpb.ColumnType) {
			if !colTypeElem.IsVirtual {
				return
			}
			col := tableElements.FilterColumnName().Filter(func(current scpb.Status, target scpb.TargetStatus, colNameElem *scpb.ColumnName) bool {
				return colNameElem.ColumnID == colTypeElem.ColumnID && target == scpb.ToPublic
			}).MustGetOneElement()
			virtualColNames = append(virtualColNames, col.Name)
		})

		kvWriterEnabled := sqlclustersettings.LDRWriterType(sqlclustersettings.LDRImmediateModeWriter.Get(&b.ClusterSettings().SV))
		if !tree.IsAllowedLDRSchemaChange(n, virtualColNames, kvWriterEnabled == sqlclustersettings.LDRWriterTypeLegacyKV) {
			_, _, ns := scpb.FindNamespace(tableElements)
			if ns == nil {
				panic(errors.AssertionFailedf("programming error: Namespace element not found"))
			}
			panic(sqlerrors.NewDisallowedSchemaChangeOnLDRTableErr(ns.Name, ldrJobIDs.JobIDs))
		}
	}
	return maybeCleanupSchemaLockedFn
}

// panicIfSystemColumn blocks alter operations on system columns.
func panicIfSystemColumn(column *scpb.Column, columnName string) {
	if column.IsSystemColumn {
		// Block alter operations on system columns.
		panic(pgerror.Newf(
			pgcode.FeatureNotSupported,
			"cannot alter system column %q", columnName))
	}
}

// panicIfRegionChangeUnderwayOnRBRTable panics if the given table is regional
// by row and any of the regions on the database of the table are currently
// being modified by another schema change job.
func panicIfRegionChangeUnderwayOnRBRTable(b BuildCtx, op redact.SafeString, tableID catid.DescID) {
	tableElems := b.QueryByID(tableID)
	_, _, rbrElem := scpb.FindTableLocalityRegionalByRow(tableElems)
	if rbrElem == nil {
		return
	}
	_, _, ns := scpb.FindNamespace(tableElems)
	dbElems := b.QueryByID(ns.DatabaseID)
	if _, _, rc := scpb.FindDatabaseRegionConfig(dbElems); rc == nil {
		return
	}
	r, err := b.SynthesizeRegionConfig(b, ns.DatabaseID)
	if err != nil {
		panic(err)
	}
	if len(r.TransitioningRegions()) > 0 {
		panic(errors.WithDetailf(
			errors.WithHintf(
				pgerror.Newf(
					pgcode.ObjectNotInPrerequisiteState,
					"cannot %s on a REGIONAL BY ROW table while a region is being added or dropped on the database",
					op,
				),
				"cancel the job which is adding or dropping the region or try again later",
			),
			"region %s is currently being added or dropped",
			r.TransitioningRegions()[0],
		))
	}
}

// haveSameIndexColsByKind returns true if two indexes have the same index
// columns of a particular kind.
func haveSameIndexColsByKind(
	b BuildCtx, tableID catid.DescID, indexID1, indexID2 catid.IndexID, kind scpb.IndexColumn_Kind,
) bool {
	tableElems := b.QueryByID(tableID)
	cols1 := getIndexColumns(tableElems, indexID1, kind)
	cols2 := getIndexColumns(tableElems, indexID2, kind)
	if len(cols1) != len(cols2) {
		return false
	}
	for i := range cols1 {
		if cols1[i].ColumnID != cols2[i].ColumnID ||
			cols1[i].Direction != cols2[i].Direction {
			return false
		}
	}
	return true
}

// haveSameIndexCols returns true if two indexes have the same index columns.
func haveSameIndexCols(b BuildCtx, tableID catid.DescID, indexID1, indexID2 catid.IndexID) bool {
	return haveSameIndexColsByKind(b, tableID, indexID1, indexID2, scpb.IndexColumn_KEY) &&
		haveSameIndexColsByKind(b, tableID, indexID1, indexID2, scpb.IndexColumn_KEY_SUFFIX) &&
		haveSameIndexColsByKind(b, tableID, indexID1, indexID2, scpb.IndexColumn_STORED)
}

// compareNumOfIndexCols compares the number of columns of `kind` in two indexes.
// The return is equal to `indexID1.numberOfColumnsOfKind - indexID2.numberOfColumnsOfKind`.
func compareNumOfIndexCols(
	b BuildCtx, tableID catid.DescID, indexID1, indexID2 catid.IndexID, kind scpb.IndexColumn_Kind,
) int {
	tableElems := b.QueryByID(tableID)
	cols1 := getIndexColumns(tableElems, indexID1, kind)
	cols2 := getIndexColumns(tableElems, indexID2, kind)
	return len(cols1) - len(cols2)
}

// getPrimaryIndexChain returns all "adding" primary indexes
// in the table and ensure they are "sorted by sourcing".
//   - "adding" means they are targeting public but currently not public;
//   - "sorted by sourcing" means if primary_index_j's source index ID points
//     to primary_index_i, then primary_index_i comes before primary_index_j.
//
// We conclude that the return has at least 1 and at most 4 primary indexes.
// To facilitate conversation, let's call them (`old`, `inter1`, `inter2`, `final`):
//   - `old` is the original, currently public primary index (it's always going
//     to exist and hence "at least 1").
//   - `inter1` is the newly added primary index that contains all the added/dropped
//     columns in this statement.
//   - `inter2` is same as `inter1` but with altered primary key.
//   - `final` is same as `inter2` but without dropped column.
//
// The following comments explain in what cases would we have 2, 3, or 4 adding
// primary indexes.
//
// Usually, if there is just one add column, or one drop column, or one
// alter primary key in one alter table statement, we will only create one new
// primary index with the correct columns. That would be `final` and we backfill
// it from `old`, that is, (`old`, nil, nil, `final`).
//
// Occasionally, we might need one intermediate primary index. It happens in the
// following two cases:
// 1). `ALTER TABLE t ALTER PRIMARY KEY where old PK is on rowid;`
// 2). `ALTER TALBE t ADD COLUMN, DROP COLUMN;`
// For 1), the intermediate primary index will be one with the altered PK but
// retaining rowid in its storing columns. The final primary index will then be
// one that drops rowid from its storing columns, so, (`old`, nil, `inter2`, `final`).
// For 2), the intermediate primary index will be one with all the added and
// dropped columns in its storing columns. Its final primary index will then be
// one that drops those to-be-dropped columns from its storing columns, so,
// (`old`, `inter1`, nil, `final`).
//
// Rarely, we would encounter something like
// `ALTER TABLE ADD COLUMN, DROP COLUMN, ALTER PRIMARY KEY;`
// To correctly build this statement, we will need two intermediate primary
// indexes where intermediate1 will be one that has all the added and dropped
// columns in its storing columns, and intermediate2 will be one that drops all
// to-be-dropped columns from its storing columns, and `final` will be one with
// altered PK, so, (`old`, `inter1`, `inter2`, `final`).
func getPrimaryIndexChain(b BuildCtx, tableID catid.DescID) *primaryIndexChain {
	// Collect all "adding" primary indexes (i.e. target public currently not public)
	// in this table and sort them by their `SourceIndexID`.
	var old, inter1, inter2, final *scpb.PrimaryIndex
	primaryIndexes := make(map[*scpb.PrimaryIndex]bool)
	scpb.ForEachPrimaryIndex(b.QueryByID(tableID).
		Filter(orFilter(publicTargetFilter, transientTargetFilter)).
		Filter(notReachedTargetYetFilter),
		func(
			current scpb.Status, target scpb.TargetStatus, e *scpb.PrimaryIndex,
		) {
			primaryIndexes[e] = true
		})
	_, _, old = scpb.FindPrimaryIndex(b.QueryByID(tableID).Filter(publicStatusFilter))
	primaryIndexes[old] = true

	// The following convoluted logic attempts to sort all adding primary indexes
	// by their SourceIndexID "locationally".
	sortedPrimaryIndexes := make([]*scpb.PrimaryIndex, len(primaryIndexes))
	sources := make(map[catid.IndexID]bool)
	for addingPrimaryIndex := range primaryIndexes {
		sources[addingPrimaryIndex.SourceIndexID] = true
	}
	for len(primaryIndexes) > 0 {
		for primaryIndex := range primaryIndexes {
			if _, ok := sources[primaryIndex.IndexID]; ok {
				// this primary index is currently used as someone else's source.
				continue
			}
			// Find the one that's nobody's source!
			// Put it to `sourtedPrimaryIndexes`, back to front.
			sortedPrimaryIndexes[len(primaryIndexes)-1] = primaryIndex
			delete(primaryIndexes, primaryIndex)
			delete(sources, primaryIndex.SourceIndexID)
			break
		}
	}

	// Sanity check: There should be at least 1, and at most 4 primary indexes.
	if len(sortedPrimaryIndexes) < 1 || len(sortedPrimaryIndexes) > 4 {
		panic(errors.AssertionFailedf("programming error: table %v has %v primary indexes; "+
			"should be between 1 and 4", tableID, len(sortedPrimaryIndexes)))
	}

	switch len(sortedPrimaryIndexes) {
	case 2:
		final = sortedPrimaryIndexes[1]
	case 3:
		final = sortedPrimaryIndexes[2]
		if haveSameIndexColsByKind(b, tableID,
			sortedPrimaryIndexes[0].IndexID, sortedPrimaryIndexes[1].IndexID, scpb.IndexColumn_KEY) {
			inter1 = sortedPrimaryIndexes[1]
		} else {
			inter2 = sortedPrimaryIndexes[1]
		}
	case 4:
		inter1 = sortedPrimaryIndexes[1]
		inter2 = sortedPrimaryIndexes[2]
		final = sortedPrimaryIndexes[3]
	}

	return NewPrimaryIndexChain(b, old, inter1, inter2, final)
}

// getPrimaryIndexID finds and returns the PrimaryIndex. If there were changes
// to the primary index in this transaction, it returns pointer to the modified
// index.
func getLatestPrimaryIndex(b BuildCtx, tableID catid.DescID) *scpb.PrimaryIndex {
	chain := getPrimaryIndexChain(b, tableID)
	if chain.finalSpec.primary != nil {
		return chain.finalSpec.primary
	} else {
		return chain.oldSpec.primary
	}
}

// addASwapInIndexByCloningFromSource adds a primary index `in` that is going
// to swap out `out` yet `in`'s columns are cloned from `source`.
//
// It might sound redundant to do so such thing ("backfilling from an index into
// another of the same columns"). Yes, but it is a prep step to facilitate us
// later to modify columns of `in`, be it an ADD COLUMN, DROP COLUMN,
// or ALTER PRIMARY KEY.
//
// `isInFinal` is set if `in` is going to be the final primary indexes, in which
// case we set its target to PUBLIC. Otherwise, `in`'s target is set to TRANSIENT.
func addASwapInIndexByCloningFromSource(
	b BuildCtx, tableID catid.DescID, out catid.IndexID, source catid.IndexID, isInFinal bool,
) (inSpec indexSpec, inTempSpec indexSpec) {
	outSpec := makeIndexSpec(b, tableID, out)

	inColumns := make([]indexColumnSpec, 0)
	fromKeyCols := getIndexColumns(b.QueryByID(tableID), source, scpb.IndexColumn_KEY)
	fromStoringCols := getIndexColumns(b.QueryByID(tableID), source, scpb.IndexColumn_STORED)
	for _, fromIndexCol := range append(fromKeyCols, fromStoringCols...) {
		inColumns = append(inColumns, makeIndexColumnSpec(fromIndexCol))
	}

	inSpec, inTempSpec = makeSwapIndexSpec(b, outSpec, out, inColumns, true /* inUseTempIDs */)
	if isInFinal {
		inSpec.apply(b.Add)
	} else {
		inSpec.apply(b.AddTransient)
	}
	inTempSpec.apply(b.AddTransient)
	return inSpec, inTempSpec
}

// updateElementsToDependOnNewFromOld finds all elements of this table that
// "depend" on index `old`, meaning they use index `old` to either backfill
// data from (for indexes) or check validity against (for constraints), and
// update them to "depend" on index `new`.
//
// Note that this function excludes acting upon indexes whose IDs are in `excludes`.
func updateElementsToDependOnNewFromOld(
	b BuildCtx, tableID catid.DescID, old catid.IndexID, new catid.IndexID, excludes catid.IndexSet,
) {
	b.QueryByID(tableID).ForEach(func(current scpb.Status, target scpb.TargetStatus, e scpb.Element) {
		switch e := e.(type) {
		case *scpb.PrimaryIndex:
			if e.SourceIndexID == old && !excludes.Contains(e.IndexID) {
				e.SourceIndexID = new
			}
		case *scpb.TemporaryIndex:
			if e.SourceIndexID == old && !excludes.Contains(e.IndexID) {
				e.SourceIndexID = new
			}
		case *scpb.SecondaryIndex:
			if e.SourceIndexID == old && !excludes.Contains(e.IndexID) {
				e.SourceIndexID = new
			}
		case *scpb.CheckConstraint:
			if e.IndexIDForValidation == old {
				e.IndexIDForValidation = new
			}
		case *scpb.ForeignKeyConstraint:
			if e.IndexIDForValidation == old {
				e.IndexIDForValidation = new
			}
		case *scpb.ColumnNotNull:
			if e.IndexIDForValidation == old {
				e.IndexIDForValidation = new
			}
		case *scpb.UniqueWithoutIndexConstraint:
			if e.IndexIDForValidation == old {
				e.IndexIDForValidation = new
			}
		}
	})
}

// primaryIndexChain holds a chain of primary indexes
// "old <-- inter1 <-- inter2 <-- final" and their corresponding
// temporary indexes as needed to fulfill certain schema changes.
type primaryIndexChain struct {
	oldSpec        indexSpec
	inter1Spec     indexSpec
	inter1TempSpec indexSpec
	inter2Spec     indexSpec
	inter2TempSpec indexSpec
	finalSpec      indexSpec
	finalTempSpec  indexSpec
}

// NewPrimaryIndexChain initializes a new primaryIndexChain.
func NewPrimaryIndexChain(
	b BuildCtx, old, inter1, inter2, final *scpb.PrimaryIndex,
) *primaryIndexChain {
	ret := &primaryIndexChain{}
	tableID := old.TableID
	ret.oldSpec = makeIndexSpec(b, tableID, old.IndexID)
	if inter1 != nil {
		ret.inter1Spec = makeIndexSpec(b, tableID, inter1.IndexID)
		ret.inter1TempSpec = makeIndexSpec(b, tableID, inter1.TemporaryIndexID)
	}
	if inter2 != nil {
		ret.inter2Spec = makeIndexSpec(b, tableID, inter2.IndexID)
		ret.inter2TempSpec = makeIndexSpec(b, tableID, inter2.TemporaryIndexID)
	}
	if final != nil {
		ret.finalSpec = makeIndexSpec(b, tableID, final.IndexID)
		ret.finalTempSpec = makeIndexSpec(b, tableID, final.TemporaryIndexID)
	}
	ret.validate()
	return ret
}

// inflate iterate over the current chain of primary indexes and inflate them to
// a chain of four, non-nil primary indexes by going from old to inter1 to
// inter2 to final and copy from the previous one if the current one is nil.
func (pic *primaryIndexChain) inflate(b BuildCtx) {
	// insertSwapInInChain is a helper function that adds a swap-in primary index
	// (and its temporary index), cloned from `source`, which will swap out `out`
	// in the chain.
	insertSwapInInChain := func(
		b BuildCtx, tableID catid.DescID, out catid.IndexID, source catid.IndexID, isInFinal bool,
	) (in, inTemp indexSpec) {
		in, inTemp = addASwapInIndexByCloningFromSource(b, tableID, out, source, isInFinal)
		updateElementsToDependOnNewFromOld(b, tableID, out, in.primary.IndexID,
			catid.MakeIndexIDSet(in.primary.IndexID, in.primary.TemporaryIndexID))
		return in, inTemp
	}

	if pic == nil {
		return
	}
	tableID := pic.oldSpec.primary.TableID
	// Mark old primary index as dropped for the very first time when we inflate.
	// We might add a column in the old index in ADD COLUMN so we don't drop the
	// old index spec everytime we inflate.
	if pic.chainType() == noNewPrimaryIndexType {
		pic.oldSpec.apply(b.Drop)
	}

	// Special handling of ADD COLUMN(s) and ALTER PK to correctly inflate the
	// chain back to the state previously before deflation.
	if pic.chainType() == oneNewPrimaryIndexType {
		// oneNewPrimaryIndexType occurs only in three cases: only add column(s), only drop column(s), or only alter PK.
		// We have special inflation logic for "only alter PK" and "only add column(s)".
		if !haveSameIndexColsByKind(b, tableID, pic.oldSpec.primary.IndexID, pic.finalSpec.primary.IndexID, scpb.IndexColumn_KEY) {
			// only ALTER PK: inflate to "old, old, final, final"
			// `inter1` is cloned from `old`, as normal.
			pic.inter1Spec, pic.inter1TempSpec = insertSwapInInChain(b, tableID,
				pic.oldSpec.primary.IndexID, pic.oldSpec.primary.IndexID, false)
			// `inter2` is cloned from `final`!
			pic.inter2Spec, pic.inter2TempSpec = insertSwapInInChain(b, tableID,
				pic.inter1Spec.primary.IndexID, pic.finalSpec.primary.IndexID, false)
		} else if compareNumOfIndexCols(b, tableID, pic.oldSpec.primary.IndexID, pic.finalSpec.primary.IndexID, scpb.IndexColumn_STORED) < 0 {
			// Only ADD COLUMN(s): inflate to "old, final, final, final"
			// `inter1` is cloned from `final`!
			pic.inter1Spec, pic.inter1TempSpec = insertSwapInInChain(b, tableID,
				pic.oldSpec.primary.IndexID, pic.finalSpec.primary.IndexID, false)
			// `inter2` will be handled by logic below where we clone from the predecessor.
		}
	}

	// General machinery to inflate a chain: from left to right, if an index is empty,
	// clone it from its predecessor.
	if pic.inter1Spec.primary == nil {
		pic.inter1Spec, pic.inter1TempSpec = insertSwapInInChain(b, tableID,
			pic.oldSpec.primary.IndexID, pic.oldSpec.primary.IndexID, false)
	}
	if pic.inter2Spec.primary == nil {
		pic.inter2Spec, pic.inter2TempSpec = insertSwapInInChain(b, tableID,
			pic.inter1Spec.primary.IndexID, pic.inter1Spec.primary.IndexID, false)
	}
	if pic.finalSpec.primary == nil {
		pic.finalSpec, pic.finalTempSpec = insertSwapInInChain(b, tableID,
			pic.inter2Spec.primary.IndexID, pic.inter2Spec.primary.IndexID, true)
	}

	// Validate we end up with a valid chain of primary indexes.
	pic.validate()
}

// deflate iterate over the current inflated chain of primary indexes and remove
// duplicate ones with the following rules:
// 1). if old == inter1, remove inter1
// 2). if final == inter2, remove inter2
// 3). if inter1 == inter2, remove the unremoved one or final if both are removed.
//
// The following is an enumeration of all 8 cases those rules imply:
// 1. (old == inter1 && inter1 == inter2 && inter2 == final), drop inter1, inter2, and final
// 2. (old == inter1 && inter1 == inter2 && inter2 != final), drop inter1 and inter2
// 3. (old == inter1 && inter1 != inter2 && inter2 == final), drop inter1 and inter2
// 4. (old == inter1 && inter1 != inter2 && inter2 != final), drop inter1
// 5. (old != inter1 && inter1 == inter2 && inter2 == final), drop inter1 and inter2
// 6. (old != inter1 && inter1 == inter2 && inter2 != final), drop inter2
// 7. (old != inter1 && inter1 != inter2 && inter2 == final), drop inter2
// 8. (old != inter1 && inter1 != inter2 && inter2 != final), do nothing
func (pic *primaryIndexChain) deflate(b BuildCtx) {
	if !pic.isFullyInflated() {
		return
	}
	tableID := pic.oldSpec.primary.TableID

	// Find redundant primary/temporary indexSpecs.
	redundants := make([]*indexSpec, 0)
	redundantIDs := make(map[*indexSpec]bool)
	markAsRedundant := func(idxSpec *indexSpec) {
		redundants = append(redundants, idxSpec)
		redundantIDs[idxSpec] = true
	}

	if haveSameIndexCols(b, tableID, pic.oldSpec.primary.IndexID, pic.inter1Spec.primary.IndexID) {
		markAsRedundant(&pic.inter1Spec)
		markAsRedundant(&pic.inter1TempSpec)
	}
	if haveSameIndexCols(b, tableID, pic.finalSpec.primary.IndexID, pic.inter2Spec.primary.IndexID) {
		markAsRedundant(&pic.inter2Spec)
		markAsRedundant(&pic.inter2TempSpec)
	}
	if haveSameIndexCols(b, tableID, pic.inter1Spec.primary.IndexID, pic.inter2Spec.primary.IndexID) {
		if _, exist := redundantIDs[&pic.inter2Spec]; !exist {
			markAsRedundant(&pic.inter2Spec)
			markAsRedundant(&pic.inter2TempSpec)
		} else if _, exist = redundantIDs[&pic.inter1Spec]; !exist {
			markAsRedundant(&pic.inter1Spec)
			markAsRedundant(&pic.inter1TempSpec)
		} else {
			// We've inflated the chain but end up needing to drop all new primary
			// indexes (e.g. adding a column that has no default value and no
			// computed expression). When we inflate a chain, we mark `old` as
			// to-be-dropped, so we need to undo it here.
			markAsRedundant(&pic.finalSpec)
			markAsRedundant(&pic.finalTempSpec)
			pic.oldSpec.apply(b.Add)
		}
	}

	// Drop those redundant primary/temporary indexSpecs.
	for _, redundant := range redundants {
		redundant.apply(b.Drop)
	}
	// Update elements after marking redundant primary indexes as dropping.
	//
	// N.B. This cannot be put inside the same for-loop above because
	// we can potentially update a redundant primary index that will be dropped
	// in a following iteration and this update can cause `b.Drop` in that following
	// iteration to fail to recognize the right element (recall an element is
	// identified by attrs defined in screl and updating SourceIndexID of a
	// primary index will cause us to fail to retrieve the element to drop).
	for _, redundant := range redundants {
		updateElementsToDependOnNewFromOld(b, tableID,
			redundant.indexID(), redundant.SourceIndexID(), catid.IndexSet{} /* excludes */)
		*redundant = indexSpec{} // reset this indexSpec in the chain
	}

	pic.validate()
}

// validate validates two aspects of the chain of all primary indexes in `pic`:
// 1. Its "type" is one of a pre-defined acceptable set (see ensureTypeIsAcceptable),
// 2. Each primary index is sourced to its first non-nil predecessor
func (pic *primaryIndexChain) validate() {
	pic.ensureTypeIsAcceptable()
	pic.ensureSortedBySourcing()
}

func (pic *primaryIndexChain) allPrimaryIndexSpecs(
	selectors ...func(*indexSpec) bool,
) (ret []*indexSpec) {
	for _, spec := range []*indexSpec{&pic.oldSpec, &pic.inter1Spec, &pic.inter2Spec, &pic.finalSpec} {
		satisfied := true
		for _, selector := range selectors {
			if !selector(spec) {
				satisfied = false
				break
			}
		}
		if satisfied {
			ret = append(ret, spec)
		}
	}
	return ret
}

func (pic *primaryIndexChain) allIndexSpecs(selectors ...func(*indexSpec) bool) (ret []*indexSpec) {
	for _, spec := range []*indexSpec{&pic.oldSpec, &pic.inter1Spec, &pic.inter1TempSpec,
		&pic.inter2Spec, &pic.inter2TempSpec, &pic.finalSpec, &pic.finalTempSpec} {
		satisfied := true
		for _, selector := range selectors {
			if !selector(spec) {
				satisfied = false
				break
			}
		}
		if satisfied {
			ret = append(ret, spec)
		}
	}
	return ret
}

// nonNilPrimaryIndexSpecSelector allows us to iterate over all non-nil, primary index specs.
func nonNilPrimaryIndexSpecSelector(spec *indexSpec) bool {
	return spec.primary != nil
}

// isFullyInflated return true if all new primary indexes are non-nil.
func (pic *primaryIndexChain) isFullyInflated() bool {
	return pic.inter1Spec.primary != nil && pic.inter2Spec.primary != nil && pic.finalSpec.primary != nil
}

// isInflatedAtAll return true if any new primary index is non-nil.
func (pic *primaryIndexChain) isInflatedAtAll() bool {
	return pic.inter1Spec.primary != nil || pic.inter2Spec.primary != nil || pic.finalSpec.primary != nil
}

// chainType returns the type of the chain.
func (pic *primaryIndexChain) chainType() (ret chainType) {
	val := 0
	if pic.oldSpec.primary != nil {
		val |= oldNonNil
	}
	if pic.inter1Spec.primary != nil {
		val |= inter1NonNil
	}
	if pic.inter2Spec.primary != nil {
		val |= inter2NonNil
	}
	if pic.finalSpec.primary != nil {
		val |= finalNonNil
	}
	switch val {
	case noNewPrimaryIndexVal:
		return noNewPrimaryIndexType
	case oneNewPrimaryIndexVal:
		return oneNewPrimaryIndexType
	case twoNewPrimaryIndexesWithAlteredPKVal:
		return twoNewPrimaryIndexesWithAlteredPKType
	case twoNewPrimaryIndexesWithAddAndDropColumnsVal:
		return twoNewPrimaryIndexesWithAddAndDropColumnsType
	case threeNewPrimaryIndexesVal:
		return threeNewPrimaryIndexesType
	default:
		return invalidType
	}
}

const (
	oldNonNil    = 1 << 0
	inter1NonNil = 1 << 1
	inter2NonNil = 1 << 2
	finalNonNil  = 1 << 3

	noNewPrimaryIndexVal                         = 1
	oneNewPrimaryIndexVal                        = 9
	twoNewPrimaryIndexesWithAlteredPKVal         = 13
	twoNewPrimaryIndexesWithAddAndDropColumnsVal = 11
	threeNewPrimaryIndexesVal                    = 15
)

// A set of five pre-defined acceptable types for primary index chains:
// 1). noNewPrimaryIndex: "old, nil, nil, nil" (e.g. no add/drop column nor alter PK)
// 2). oneNewPrimaryIndex: "old, nil, nil, final" (e.g. add column(s), or drop columns(s), or alter PK without rowid)
// 3). twoNewPrimaryIndexesWithAlteredPK: "old, nil, inter2, final" (e.g. alter PK with rowid, or alter PK + drop column(s))
// 4). twoNewPrimaryIndexesWithAddAndDropColumns: "old, inter1, nil, final" (e.g. add & drop column(s))
// 5). threeNewPrimaryIndexes: "old, inter1, inter2, final" (e.g. add/drop column + alter PK)
type chainType int

const (
	noNewPrimaryIndexType chainType = iota
	oneNewPrimaryIndexType
	twoNewPrimaryIndexesWithAlteredPKType
	twoNewPrimaryIndexesWithAddAndDropColumnsType
	threeNewPrimaryIndexesType
	invalidType
)

func (pic *primaryIndexChain) ensureTypeIsAcceptable() {
	ct := pic.chainType()
	if ct == invalidType {
		panic(errors.AssertionFailedf("chain is not of an acceptable type"))
	}
}

func (pic *primaryIndexChain) ensureSortedBySourcing() {
	// Primary indexes in the chain correctly have its first non-nil predecessor
	// as source.
	nonNilPrimaryIndexSpecs := pic.allPrimaryIndexSpecs(nonNilPrimaryIndexSpecSelector)
	for i, spec := range nonNilPrimaryIndexSpecs {
		if i == 0 {
			continue
		}
		predecessorID := nonNilPrimaryIndexSpecs[i-1].primary.IndexID
		if spec.primary.SourceIndexID != predecessorID {
			panic(errors.AssertionFailedf("primary index (%v)'s source index ID %v is not equal "+
				"to its first non-nil primary index predecessor (%v)", spec.primary.IndexID,
				spec.primary.SourceIndexID, predecessorID))
		}
		tempIndexSpec := pic.mustGetIndexSpecByID(spec.primary.TemporaryIndexID)
		if tempIndexSpec.temporary.SourceIndexID != predecessorID {
			panic(errors.AssertionFailedf("primary index (%v)'s temporary index (%v)'s source "+
				"indexes ID %v is not equal to its first non-nil primary index predecessor (%v)",
				spec.primary.IndexID, spec.primary.TemporaryIndexID,
				tempIndexSpec.temporary.SourceIndexID, predecessorID))
		}
	}
}

func (pic *primaryIndexChain) mustGetIndexSpecByID(id catid.IndexID) *indexSpec {
	for _, spec := range pic.allIndexSpecs() {
		if spec.indexID() == id {
			return spec
		}
	}
	panic(errors.AssertionFailedf("no index spec with ID %v exists in the chain", id))
}

// getInflatedPrimaryIndexChain ensures we have four non nil primary indexes,
// and they have been accordingly dropped and added in the builder state.
// They are constructed from previous ADD COLUMN, DROP COLUMN,
// or ALTER PRIMARY KEY commands. If any of them is nil, it will be created
// in this function by cloning from the previous one, so that when this function
// returns, we have a valid but possibly redundant sequence of primary indexes,
// where each one is sourcing from its precedent, to achieve the schema change.
func getInflatedPrimaryIndexChain(b BuildCtx, tableID catid.DescID) (chain *primaryIndexChain) {
	chain = getPrimaryIndexChain(b, tableID)
	chain.inflate(b)
	return chain
}

// shouldRestrictAccessToSystemInterface decides whether to restrict
// access to certain SQL features from the system tenant/interface.
// This restriction exists to prevent UX surprise. See the docstring
// on the RestrictAccessToSystemInterface cluster setting for details.
//
// It is copied from legacy schema changer.
func shouldRestrictAccessToSystemInterface(
	b BuildCtx, operation, alternateAction redact.RedactableString,
) error {
	if b.Codec().ForSystemTenant() &&
		!b.SessionData().Internal && // We only restrict access for external SQL sessions.
		sqlclustersettings.RestrictAccessToSystemInterface.Get(&b.ClusterSettings().SV) {
		return errors.WithHintf(
			pgerror.Newf(pgcode.InsufficientPrivilege, "blocked %s from the system interface", operation),
			"Access blocked via %s to prevent likely user errors.\n"+
				"Try %s from a virtual cluster instead.",
			sqlclustersettings.RestrictAccessToSystemInterface.Name(),
			alternateAction)
	}
	return nil
}

// resolveTemporaryStatus checks for the pg_temp naming convention from
// Postgres, where qualifying an object name with pg_temp is equivalent to
// explicitly specifying TEMP/TEMPORARY in the CREATE syntax.
// resolveTemporaryStatus returns true if either(or both) of these conditions
// are true.
func resolveTemporaryStatus(name tree.ObjectNamePrefix, persistence tree.Persistence) bool {
	// An explicit schema can only be provided in the CREATE TEMP statement
	// iff it is pg_temp.
	if persistence.IsTemporary() && name.ExplicitSchema && name.SchemaName != catconstants.PgTempSchemaName {
		panic(pgerror.New(pgcode.InvalidTableDefinition, "cannot create temporary relation in non-temporary schema"))
	}
	return name.SchemaName == catconstants.PgTempSchemaName || persistence.IsTemporary()
}

// MaybeCreateOrResolveTemporarySchema attempts to resolve an existing temporary
// schema for the current session. If one doesn't exist, it creates a new
// temporary schema. It returns the database elements and schema elements for
// the temporary schema. This function will panic if temporary tables are not
// enabled.
func MaybeCreateOrResolveTemporarySchema(
	b BuildCtx,
) (dbElts ElementResultSet, schemaElts ElementResultSet) {
	if !b.EvalCtx().SessionData().TempTablesEnabled {
		panic(errors.WithTelemetry(
			pgerror.WithCandidateCode(
				errors.WithHint(
					errors.WithIssueLink(
						errors.Newf("temporary tables are only supported experimentally"),
						errors.IssueLink{IssueURL: build.MakeIssueURL(46260)},
					),
					"You can enable temporary tables by running `SET experimental_enable_temp_tables = 'on'`.",
				),
				pgcode.ExperimentalFeature,
			),
			"sql.schema.temp_tables_disabled",
		))
	}
	// Attempt to resolve the existing temporary schema first.
	schemaName := b.TemporarySchemaName()
	prefix := tree.ObjectNamePrefix{
		SchemaName:     tree.Name(schemaName),
		ExplicitSchema: true,
	}
	tempSchemaName := &tree.ObjectNamePrefix{
		SchemaName:     tree.Name(schemaName),
		ExplicitSchema: true,
	}
	// Resolve the current database, which will contain this new temporary schema
	// in the namespace table.
	b.ResolveDatabasePrefix(tempSchemaName)
	dbElts = b.ResolveDatabase(tree.Name(tempSchemaName.Catalog()), ResolveParams{RequiredPrivilege: privilege.CREATE})
	dbElem := dbElts.FilterDatabase().MustGetOneElement()
	schemaElts = b.ResolveSchema(prefix, ResolveParams{IsExistenceOptional: true,
		RequireOwnership:  false,
		RequiredPrivilege: 0})
	if schemaElts != nil {
		return dbElts, schemaElts
	}
	// Temporary schema didn't resolve, so lets create a new one.
	schemaDescID := b.GenerateUniqueDescID()
	b.Add(&scpb.Schema{
		SchemaID:    schemaDescID,
		IsTemporary: true,
	})
	b.Add(&scpb.SchemaParent{
		SchemaID:         schemaDescID,
		ParentDatabaseID: dbElem.DatabaseID,
	})
	b.Add(&scpb.Namespace{
		DatabaseID:   dbElem.DatabaseID,
		SchemaID:     0,
		DescriptorID: schemaDescID,
		Name:         schemaName,
	})
	return dbElts, b.QueryByID(schemaDescID)
}

func newTypeT(t *types.T) scpb.TypeT {
	return scpb.TypeT{
		Type:          t,
		ClosedTypeIDs: typedesc.GetTypeDescriptorClosure(t).Ordered(),
		TypeName:      t.SQLString(),
	}
}

func retrieveColumnTypeElem(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID,
) *scpb.ColumnType {
	_, _, ret := scpb.FindColumnType(b.QueryByID(tableID).Filter(hasColumnIDAttrFilter(columnID)))
	return ret
}

// retrieveColumnComputeExpression returns the compute expression of the column.
// If no expression exists, then nil is returned. This will handle older
// versions that may store the expression as part of the ColumnType.
func retrieveColumnComputeExpression(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID,
) (expr *scpb.Expression) {
	// First try to retrieve the expression from the ColumnComputeExpression. This
	// may be unavailable because the column doesn't have a compute expression, or
	// it's an older version that stores the expression as part of the ColumnType.
	colComputeExpression := b.QueryByID(tableID).FilterColumnComputeExpression().Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.ColumnComputeExpression) bool {
		return e.ColumnID == columnID
	}).MustGetZeroOrOneElement()
	if colComputeExpression != nil {
		return &colComputeExpression.Expression
	}
	// Check the ColumnType in case this is an older version.
	columnType := mustRetrieveColumnTypeElem(b, tableID, columnID)
	return columnType.ComputeExpr
}

// mustRetrieveColumnTypeElem retrieves the index column elements associated
// with the given indexID.
func mustRetrieveIndexColumnElements(
	b BuildCtx, tableID catid.DescID, indexID catid.IndexID,
) []*scpb.IndexColumn {
	// Get the index columns for indexID.
	var idxCols []*scpb.IndexColumn
	b.QueryByID(tableID).FilterIndexColumn().
		Filter(func(current scpb.Status, target scpb.TargetStatus, e *scpb.IndexColumn) bool {
			return e.IndexID == indexID
		}).ForEach(func(current scpb.Status, target scpb.TargetStatus, e *scpb.IndexColumn) {
		idxCols = append(idxCols, e)
	})
	if len(idxCols) == 0 {
		panic(errors.AssertionFailedf("programming error: cannot find a IndexColumn "+
			"element for index ID %v", indexID))
	}
	return idxCols
}

// mustRetrievePhysicalTableElem will resolve a tableID to a physical table
// element. A "physical" table element includes tables, views, and sequences.
func mustRetrievePhysicalTableElem(b BuildCtx, descID catid.DescID) scpb.Element {
	return b.QueryByID(descID).Filter(func(
		_ scpb.Status, _ scpb.TargetStatus, e scpb.Element,
	) bool {
		switch e := e.(type) {
		case *scpb.Table:
			return e.TableID == descID
		case *scpb.View:
			if e.IsMaterialized {
				return e.ViewID == descID
			}
		case *scpb.Sequence:
			return e.SequenceID == descID
		}
		return false
	}).MustGetOneElement()
}

// mustRetrieveIndexNameElem will resolve a tableID and indexID to an index name
// element.
func mustRetrieveIndexNameElem(
	b BuildCtx, tableID catid.DescID, indexID catid.IndexID,
) *scpb.IndexName {
	return b.QueryByID(tableID).FilterIndexName().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.IndexName) bool {
			return e.IndexID == indexID
		}).MustGetOneElement()
}

func mustRetrieveColumnName(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID,
) *scpb.ColumnName {
	return b.QueryByID(tableID).FilterColumnName().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.ColumnName) bool { return e.ColumnID == columnID }).
		MustGetOneElement()
}

func retrieveColumnNotNull(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID,
) *scpb.ColumnNotNull {
	return b.QueryByID(tableID).FilterColumnNotNull().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.ColumnNotNull) bool { return e.ColumnID == columnID }).
		MustGetZeroOrOneElement()
}

func retrieveColumnComment(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID,
) *scpb.ColumnComment {
	return b.QueryByID(tableID).FilterColumnComment().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.ColumnComment) bool { return e.ColumnID == columnID }).
		MustGetZeroOrOneElement()
}

// mustRetrievePartitioningFromIndexPartitioning retrieves the partitioning
// from the index partitioning element associated with the given tableID
// and indexID.
func mustRetrievePartitioningFromIndexPartitioning(
	b BuildCtx, tableID catid.DescID, indexID catid.IndexID,
) catalog.Partitioning {
	idxPart := b.QueryByID(tableID).FilterIndexPartitioning().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.IndexPartitioning) bool {
			return e.IndexID == indexID
		}).MustGetZeroOrOneElement()
	partition := tabledesc.NewPartitioning(nil)
	if idxPart != nil {
		partition = tabledesc.NewPartitioning(&idxPart.PartitioningDescriptor)
	}
	return partition
}

// failIfSafeUpdates checks if the sql_safe_updates is present, and if so, it
// will fail the operation.
func failIfSafeUpdates(b BuildCtx, n tree.NodeFormatter) {
	if b.SessionData().SafeUpdates {
		var errorWithMessage error
		switch n.(type) {
		case *tree.AlterTableAlterColumnType:
			errorWithMessage = errors.New("ALTER COLUMN TYPE requiring data rewrite may result in data loss " +
				"for certain type conversions or when applying a USING clause")
		case *tree.DropIndex:
			errorWithMessage = errors.New("DROP INDEX")
		default:
			panic(errors.AssertionFailedf("programming error: unexpected node type %T", n))
		}

		panic(
			pgerror.WithCandidateCode(
				errors.WithMessage(
					errorWithMessage,
					"rejected (sql_safe_updates = true)",
				),
				pgcode.Warning,
			),
		)
	}
}

func hasSubzonesForIndex(b BuildCtx, tableID descpb.ID, indexID catid.IndexID) bool {
	numIdxSubzones := b.QueryByID(tableID).FilterIndexZoneConfig().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.IndexZoneConfig) bool {
			return e.IndexID == indexID
		}).Size()
	numPartSubzones := b.QueryByID(tableID).FilterPartitionZoneConfig().
		Filter(func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.PartitionZoneConfig) bool {
			return e.IndexID == indexID
		}).Size()
	return numIdxSubzones > 0 || numPartSubzones > 0
}
