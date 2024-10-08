// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package timeutil

// FullTimeFormat is the time format used to display any unknown timestamp
// type, and always shows the full time zone offset.
const FullTimeFormat = "2006-01-02 15:04:05.999999-07:00:00"

// TimestampWithTZFormat is the time format used to display
// timestamps with a time zone offset. The minutes and seconds
// offsets are only added if they are non-zero.
const TimestampWithTZFormat = "2006-01-02 15:04:05.999999-07"

// TimestampWithoutTZFormat is the time format used to display
// timestamps without a time zone offset. The minutes and seconds
// offsets are only added if they are non-zero.
const TimestampWithoutTZFormat = "2006-01-02 15:04:05.999999"

// TimeWithTZFormat is the time format used to display a time
// with a time zone offset.
const TimeWithTZFormat = "15:04:05.999999-07"

// TimeWithoutTZFormat is the time format used to display a time
// without a time zone offset.
const TimeWithoutTZFormat = "15:04:05.999999"

// DateFormat is the time format used to display a date.
const DateFormat = "2006-01-02"
