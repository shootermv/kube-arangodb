//
// DISCLAIMER
//
// Copyright 2017 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package velocypack

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
)

// BuilderOptions contains options that influence how Builder builds slices.
type BuilderOptions struct {
	BuildUnindexedArrays     bool
	BuildUnindexedObjects    bool
	CheckAttributeUniqueness bool
}

// Builder is used to build VPack structures.
type Builder struct {
	BuilderOptions
	buf        builderBuffer
	stack      builderStack
	index      []indexVector
	keyWritten bool
}

func NewBuilder(capacity uint) *Builder {
	b := &Builder{
		buf: make(builderBuffer, 0, capacity),
	}
	return b
}

// Clear and start from scratch:
func (b *Builder) Clear() {
	b.buf = nil
	b.stack.Clear()
	b.keyWritten = false
}

// Bytes return the generated bytes.
// The returned slice is shared with the builder itself, so you must not modify it.
// When the builder is not closed, an error is returned.
func (b *Builder) Bytes() ([]byte, error) {
	if !b.IsClosed() {
		return nil, WithStack(BuilderNotClosedError)
	}
	return b.buf, nil
}

// Slice returns a slice of the result.
func (b *Builder) Slice() (Slice, error) {
	if b.buf.IsEmpty() {
		return Slice{}, nil
	}
	bytes, err := b.Bytes()
	return bytes, WithStack(err)
}

// WriteTo writes the generated bytes to the given writer.
// When the builder is not closed, an error is returned.
func (b *Builder) WriteTo(w io.Writer) (int64, error) {
	if !b.IsClosed() {
		return 0, WithStack(BuilderNotClosedError)
	}
	if n, err := w.Write(b.buf); err != nil {
		return 0, WithStack(err)
	} else {
		return int64(n), nil
	}
}

// Size returns the actual size of the generated slice.
// Returns an error when builder is not closed.
func (b *Builder) Size() (ValueLength, error) {
	if !b.IsClosed() {
		return 0, WithStack(BuilderNotClosedError)
	}
	return b.buf.Len(), nil
}

// IsEmpty returns true when no bytes have been generated yet.
func (b *Builder) IsEmpty() bool {
	return b.buf.IsEmpty()
}

// IsOpenObject returns true when the builder has an open object at the top of the stack.
func (b *Builder) IsOpenObject() bool {
	if b.stack.IsEmpty() {
		return false
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	return h == 0x0b || h == 0x014
}

// IsOpenArray returns true when the builder has an open array at the top of the stack.
func (b *Builder) IsOpenArray() bool {
	if b.stack.IsEmpty() {
		return false
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	return h == 0x06 || h == 0x013
}

// OpenObject starts a new object.
// This must be closed using Close.
func (b *Builder) OpenObject(unindexed ...bool) error {
	var vType byte
	if optionalBool(unindexed, false) {
		vType = 0x14
	} else {
		vType = 0x0b
	}
	return WithStack(b.openCompoundValue(vType))
}

// OpenArray starts a new array.
// This must be closed using Close.
func (b *Builder) OpenArray(unindexed ...bool) error {
	var vType byte
	if optionalBool(unindexed, false) {
		vType = 0x13
	} else {
		vType = 0x06
	}
	return WithStack(b.openCompoundValue(vType))
}

// Close ends an open object or array.
func (b *Builder) Close() error {
	if b.IsClosed() {
		return WithStack(BuilderNeedOpenCompoundError)
	}
	tos, _ := b.stack.Tos()
	head := b.buf[tos]

	vpackAssert(head == 0x06 || head == 0x0b || head == 0x13 || head == 0x14)

	isArray := (head == 0x06 || head == 0x13)
	index := b.index[b.stack.Len()-1]

	if index.IsEmpty() {
		b.closeEmptyArrayOrObject(tos, isArray)
		return nil
	}

	// From now on index.size() > 0
	vpackAssert(len(index) > 0)

	// check if we can use the compact Array / Object format
	if head == 0x13 || head == 0x14 ||
		(head == 0x06 && b.BuilderOptions.BuildUnindexedArrays) ||
		(head == 0x0b && (b.BuilderOptions.BuildUnindexedObjects || len(index) == 1)) {
		if b.closeCompactArrayOrObject(tos, isArray, index) {
			return nil
		}
		// This might fall through, if closeCompactArrayOrObject gave up!
	}

	if isArray {
		b.closeArray(tos, index)
		return nil
	}

	// From now on we're closing an object

	// fix head byte in case a compact Array / Object was originally requested
	b.buf[tos] = 0x0b

	// First determine byte length and its format:
	offsetSize := uint(8)
	// can be 1, 2, 4 or 8 for the byte width of the offsets,
	// the byte length and the number of subvalues:
	if b.buf.Len()-tos+ValueLength(len(index))-6 <= 0xff {
		// We have so far used _pos - tos bytes, including the reserved 8
		// bytes for byte length and number of subvalues. In the 1-byte number
		// case we would win back 6 bytes but would need one byte per subvalue
		// for the index table
		offsetSize = 1

		// Maybe we need to move down data:
		targetPos := ValueLength(3)
		if b.buf.Len() > (tos + 9) {
			_len := ValueLength(b.buf.Len() - (tos + 9))
			checkOverflow(_len)
			src := b.buf[tos+9:]
			copy(b.buf[tos+targetPos:], src[:_len])
		}
		diff := ValueLength(9 - targetPos)
		b.buf.Shrink(uint(diff))
		n := len(index)
		for i := 0; i < n; i++ {
			index[i] -= diff
		}

		// One could move down things in the offsetSize == 2 case as well,
		// since we only need 4 bytes in the beginning. However, saving these
		// 4 bytes has been sacrificed on the Altar of Performance.
	} else if b.buf.Len()-tos+2*ValueLength(len(index)) <= 0xffff {
		offsetSize = 2
	} else if b.buf.Len()-tos+4*ValueLength(len(index)) <= 0xffffffff {
		offsetSize = 4
	}

	// Now build the table:
	extraSpace := offsetSize * uint(len(index))
	if offsetSize == 8 {
		extraSpace += 8
	}
	b.buf.ReserveSpace(extraSpace)
	tableBase := b.buf.Len()
	b.buf.Grow(offsetSize * uint(len(index)))
	// Object
	if len(index) >= 2 {
		if err := b.sortObjectIndex(b.buf[tos:], index); err != nil {
			return WithStack(err)
		}
	}
	for i := uint(0); i < uint(len(index)); i++ {
		indexBase := tableBase + ValueLength(offsetSize*i)
		x := uint64(index[i])
		for j := uint(0); j < offsetSize; j++ {
			b.buf[indexBase+ValueLength(j)] = byte(x & 0xff)
			x >>= 8
		}
	}
	// Finally fix the byte width in the type byte:
	if offsetSize > 1 {
		if offsetSize == 2 {
			b.buf[tos] += 1
		} else if offsetSize == 4 {
			b.buf[tos] += 2
		} else { // offsetSize == 8
			b.buf[tos] += 3
			b.appendLength(ValueLength(len(index)), 8)
		}
	}

	// Fix the byte length in the beginning:
	x := ValueLength(b.buf.Len() - tos)
	for i := uint(1); i <= offsetSize; i++ {
		b.buf[tos+ValueLength(i)] = byte(x & 0xff)
		x >>= 8
	}

	if offsetSize < 8 {
		x := len(index)
		for i := uint(offsetSize + 1); i <= 2*offsetSize; i++ {
			b.buf[tos+ValueLength(i)] = byte(x & 0xff)
			x >>= 8
		}
	}

	// And, if desired, check attribute uniqueness:
	if b.BuilderOptions.CheckAttributeUniqueness && len(index) > 1 {
		// check uniqueness of attribute names
		if err := b.checkAttributeUniqueness(Slice(b.buf[tos:])); err != nil {
			return WithStack(err)
		}
	}

	// Now the array or object is complete, we pop a ValueLength off the _stack:
	b.stack.Pop()
	// Intentionally leave _index[depth] intact to avoid future allocs!
	return nil
}

// IsClosed returns true if there are no more open objects or arrays.
func (b *Builder) IsClosed() bool {
	return b.stack.IsEmpty()
}

// HasKey checks whether an Object value has a specific key attribute.
func (b *Builder) HasKey(key string) (bool, error) {
	if b.stack.IsEmpty() {
		return false, WithStack(BuilderNeedOpenObjectError)
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	if h != 0x0b && h != 0x14 {
		return false, WithStack(BuilderNeedOpenObjectError)
	}
	index := b.index[b.stack.Len()-1]
	if index.IsEmpty() {
		return false, nil
	}
	for _, idx := range index {
		s := Slice(b.buf[tos+idx:])
		k, err := s.makeKey()
		if err != nil {
			return false, WithStack(err)
		}
		if eq, err := k.IsEqualString(key); err != nil {
			return false, WithStack(err)
		} else if eq {
			return true, nil
		}
	}
	return false, nil
}

// GetKey returns the value for a specific key of an Object value.
// Returns Slice of type None when key is not found.
func (b *Builder) GetKey(key string) (Slice, error) {
	if b.stack.IsEmpty() {
		return nil, WithStack(BuilderNeedOpenObjectError)
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	if h != 0x0b && h != 0x14 {
		return nil, WithStack(BuilderNeedOpenObjectError)
	}
	index := b.index[b.stack.Len()-1]
	if index.IsEmpty() {
		return nil, nil
	}
	for _, idx := range index {
		s := Slice(b.buf[tos+idx:])
		k, err := s.makeKey()
		if err != nil {
			return nil, WithStack(err)
		}
		if eq, err := k.IsEqualString(key); err != nil {
			return nil, WithStack(err)
		} else if eq {
			value, err := s.Next()
			if err != nil {
				return nil, WithStack(err)
			}
			return value, nil
		}
	}
	return nil, nil
}

// RemoveLast removes last subvalue written to an (unclosed) object or array.
func (b *Builder) RemoveLast() error {
	if b.stack.IsEmpty() {
		return WithStack(BuilderNeedOpenCompoundError)
	}
	tos, _ := b.stack.Tos()
	index := &b.index[b.stack.Len()-1]
	if index.IsEmpty() {
		return WithStack(BuilderNeedSubValueError)
	}
	newLength := tos + (*index)[len(*index)-1]
	lastSize := b.buf.Len() - newLength
	b.buf.Shrink(uint(lastSize))
	index.RemoveLast()
	return nil
}

// addNull adds a null value to the buffer.
func (b *Builder) addNull() {
	b.buf.WriteByte(0x18)
}

// addFalse adds a bool false value to the buffer.
func (b *Builder) addFalse() {
	b.buf.WriteByte(0x19)
}

// addTrue adds a bool true value to the buffer.
func (b *Builder) addTrue() {
	b.buf.WriteByte(0x1a)
}

// addBool adds a bool value to the buffer.
func (b *Builder) addBool(v bool) {
	if v {
		b.addTrue()
	} else {
		b.addFalse()
	}
}

// addDouble adds a double value to the buffer.
func (b *Builder) addDouble(v float64) {
	bits := math.Float64bits(v)
	b.buf.ReserveSpace(9)
	b.buf.WriteByte(0x1b)
	binary.LittleEndian.PutUint64(b.buf.Grow(8), bits)
}

// addInt adds an int value to the buffer.
func (b *Builder) addInt(v int64) {
	if v >= 0 && v <= 9 {
		b.buf.WriteByte(0x30 + byte(v))
	} else if v < 0 && v >= -6 {
		b.buf.WriteByte(byte(0x40 + int(v)))
	} else {
		b.appendInt(v, 0x1f)
	}
}

// addUInt adds an uint value to the buffer.
func (b *Builder) addUInt(v uint64) {
	if v <= 9 {
		b.buf.WriteByte(0x30 + byte(v))
	} else {
		b.appendUInt(v, 0x27)
	}
}

// addUTCDate adds an UTC date value to the buffer.
func (b *Builder) addUTCDate(v int64) {
	x := toUInt64(v)
	dst := b.buf.Grow(9)
	dst[0] = 0x1c
	setLength(dst[1:], ValueLength(x), 8)
}

// addString adds a string value to the buffer.
func (b *Builder) addString(v string) {
	strLen := uint(len(v))
	if strLen > 126 {
		// long string
		dst := b.buf.Grow(1 + 8 + strLen)
		dst[0] = 0xbf
		setLength(dst[1:], ValueLength(strLen), 8) // string length
		copy(dst[9:], v)                           // string data
	} else {
		dst := b.buf.Grow(1 + strLen)
		dst[0] = byte(0x40 + strLen) // short string (with length)
		copy(dst[1:], v)             // string data
	}
}

// addBinary adds a binary value to the buffer.
func (b *Builder) addBinary(v []byte) {
	l := uint(len(v))
	b.buf.ReserveSpace(1 + 8 + l)
	b.appendUInt(uint64(l), 0xbf) // data length
	b.buf.Write(v)                // data
}

// addIllegal adds an Illegal value to the buffer.
func (b *Builder) addIllegal() {
	b.buf.WriteByte(0x17)
}

// addMinKey adds a MinKey value to the buffer.
func (b *Builder) addMinKey() {
	b.buf.WriteByte(0x1e)
}

// addMaxKey adds a MaxKey value to the buffer.
func (b *Builder) addMaxKey() {
	b.buf.WriteByte(0x1f)
}

// Add adds a raw go value value to an array/raw value/object.
func (b *Builder) Add(v interface{}) error {
	if it, ok := v.(*ObjectIterator); ok {
		return WithStack(b.AddKeyValuesFromIterator(it))
	}
	if it, ok := v.(*ArrayIterator); ok {
		return WithStack(b.AddValuesFromIterator(it))
	}
	value := NewValue(v)
	if value.IsIllegal() {
		return WithStack(BuilderUnexpectedTypeError{fmt.Sprintf("Cannot convert value of type %s", reflect.TypeOf(v).Name())})
	}
	if err := b.addInternal(value); err != nil {
		return WithStack(err)
	}
	return nil
}

// AddValue adds a value to an array/raw value/object.
func (b *Builder) AddValue(v Value) error {
	if err := b.addInternal(v); err != nil {
		return WithStack(err)
	}
	return nil
}

// AddKeyValue adds a key+value to an open object.
func (b *Builder) AddKeyValue(key string, v Value) error {
	if err := b.addInternalKeyValue(key, v); err != nil {
		return WithStack(err)
	}
	return nil
}

// AddValuesFromIterator adds values to an array from the given iterator.
// The array must be opened before a call to this function and the array is left open Intentionally.
func (b *Builder) AddValuesFromIterator(it *ArrayIterator) error {
	if b.stack.IsEmpty() {
		return WithStack(BuilderNeedOpenArrayError)
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	if h != 0x06 && h != 0x13 {
		return WithStack(BuilderNeedOpenArrayError)
	}
	for it.IsValid() {
		v, err := it.Value()
		if err != nil {
			return WithStack(err)
		}
		if err := b.addInternal(NewSliceValue(v)); err != nil {
			return WithStack(err)
		}
		if err := it.Next(); err != nil {
			return WithStack(err)
		}
	}
	return nil
}

// AddKeyValuesFromIterator adds values to an object from the given iterator.
// The object must be opened before a call to this function and the object is left open Intentionally.
func (b *Builder) AddKeyValuesFromIterator(it *ObjectIterator) error {
	if b.stack.IsEmpty() {
		return WithStack(BuilderNeedOpenObjectError)
	}
	tos, _ := b.stack.Tos()
	h := b.buf[tos]
	if h != 0x0b && h != 0x14 {
		return WithStack(BuilderNeedOpenObjectError)
	}
	if b.keyWritten {
		return WithStack(BuilderKeyAlreadyWrittenError)
	}
	for it.IsValid() {
		k, err := it.Key(true)
		if err != nil {
			return WithStack(err)
		}
		key, err := k.GetString()
		if err != nil {
			return WithStack(err)
		}
		v, err := it.Value()
		if err != nil {
			return WithStack(err)
		}
		if err := b.addInternalKeyValue(key, NewSliceValue(v)); err != nil {
			return WithStack(err)
		}
		if err := it.Next(); err != nil {
			return WithStack(err)
		}
	}
	return nil
}

// returns number of bytes required to store the value in 2s-complement
func intLength(value int64) uint {
	if value >= -0x80 && value <= 0x7f {
		// shortcut for the common case
		return 1
	}
	var x uint64
	if value >= 0 {
		x = uint64(value)
	} else {
		x = uint64(-(value + 1))
	}
	xSize := uint(0)
	for {
		xSize++
		x >>= 8
		if x < 0x80 {
			return xSize + 1
		}
	}
}

func (b *Builder) appendInt(v int64, base uint) {
	vSize := intLength(v)
	var x uint64
	if vSize == 8 {
		x = toUInt64(v)
	} else {
		shift := int64(1) << (vSize*8 - 1) // will never overflow!
		if v >= 0 {
			x = uint64(v)
		} else {
			x = uint64(v+shift) + uint64(shift)
		}
		//      x = v >= 0 ? static_cast<uint64_t>(v)
		//                 : static_cast<uint64_t>(v + shift) + shift;
	}
	dst := b.buf.Grow(1 + vSize)
	dst[0] = byte(base + vSize)
	off := 1
	for ; vSize > 0; vSize-- {
		dst[off] = byte(x & 0xff)
		x >>= 8
		off++
	}
}

func (b *Builder) appendUInt(v uint64, base uint) {
	b.buf.ReserveSpace(9)
	save := b.buf.Len()
	b.buf.WriteByte(0) // Will be overwritten at end of function.
	vSize := uint(0)
	for {
		vSize++
		b.buf.WriteByte(byte(v & 0xff))
		v >>= 8
		if v == 0 {
			break
		}
	}
	b.buf[save] = byte(base + vSize)
}

func (b *Builder) appendLength(v ValueLength, n uint) {
	dst := b.buf.Grow(n)
	setLength(dst, v, n)
}

func setLength(dst []byte, v ValueLength, n uint) {
	for i := uint(0); i < n; i++ {
		dst[i] = byte(v & 0xff)
		v >>= 8
	}
}

// openCompoundValue opens an array/object, checking the context.
func (b *Builder) openCompoundValue(vType byte) error {
	//haveReported := false
	tos, stackLen := b.stack.Tos()
	if stackLen > 0 {
		h := b.buf[tos]
		if !b.keyWritten {
			if h != 0x06 && h != 0x13 {
				return WithStack(BuilderNeedOpenArrayError)
			}
			b.reportAdd()
			//haveReported = true
		} else {
			b.keyWritten = false
		}
	}
	b.addCompoundValue(vType)
	// if err && haveReported { b.cleanupAdd() }
	return nil
}

// addCompoundValue adds the start of a component value to the stream & stack.
func (b *Builder) addCompoundValue(vType byte) {
	pos := b.buf.Len()
	b.stack.Push(pos)
	stackLen := b.stack.Len()
	toAdd := stackLen - len(b.index)
	for toAdd > 0 {
		newIndex := make(indexVector, 0, 16) // Pre-allocate 16 entries so we don't have to allocate memory for the first 16 entries
		b.index = append(b.index, newIndex)
		toAdd--
	}
	b.index[stackLen-1].Clear()
	dst := b.buf.Grow(9)
	dst[0] = vType
	//b.buf.WriteBytes(0, 8) // Will be filled later with bytelength and nr subs
}

// closeEmptyArrayOrObject closes an empty array/object, removing the pre-allocated length space.
func (b *Builder) closeEmptyArrayOrObject(tos ValueLength, isArray bool) {
	// empty Array or Object
	if isArray {
		b.buf[tos] = 0x01
	} else {
		b.buf[tos] = 0x0a
	}
	vpackAssert(b.buf.Len() == tos+9)
	b.buf.Shrink(8)
	b.stack.Pop()
}

// closeCompactArrayOrObject tries to close an array/object using compact notation.
// Returns true when a compact notation was possible, false otherwise.
func (b *Builder) closeCompactArrayOrObject(tos ValueLength, isArray bool, index indexVector) bool {
	// use compact notation
	nrItems := len(index)
	nrItemsLen := getVariableValueLength(ValueLength(nrItems))
	vpackAssert(nrItemsLen > 0)

	byteSize := b.buf.Len() - (tos + 8) + nrItemsLen
	vpackAssert(byteSize > 0)

	byteSizeLen := getVariableValueLength(byteSize)
	byteSize += byteSizeLen
	if getVariableValueLength(byteSize) != byteSizeLen {
		byteSize++
		byteSizeLen++
	}

	if byteSizeLen < 9 {
		// can only use compact notation if total byte length is at most 8 bytes long
		if isArray {
			b.buf[tos] = 0x13
		} else {
			b.buf[tos] = 0x14
		}

		valuesLen := b.buf.Len() - (tos + 9) // Amount of bytes taken up by array/object values.
		if valuesLen > 0 && byteSizeLen < 8 {
			// We have array/object values and our byteSize needs less than the pre-allocated 8 bytes.
			// So we move the array/object values back.
			checkOverflow(valuesLen)
			src := b.buf[tos+9:]
			copy(b.buf[tos+1+byteSizeLen:], src[:valuesLen])
		}
		// Shrink buffer, removing unused space allocated for byteSize.
		b.buf.Shrink(uint(8 - byteSizeLen))

		// store byte length
		vpackAssert(byteSize > 0)
		storeVariableValueLength(b.buf, tos+1, byteSize, false)

		// store nrItems
		b.buf.Grow(uint(nrItemsLen))
		storeVariableValueLength(b.buf, tos+byteSize-1, ValueLength(len(index)), true)

		b.stack.Pop()
		return true
	}
	return false
}

// checkAttributeUniqueness checks the given slice for duplicate keys.
// It returns an error when duplicate keys are found, nil otherwise.
func (b *Builder) checkAttributeUniqueness(obj Slice) error {
	vpackAssert(b.BuilderOptions.CheckAttributeUniqueness)
	n, err := obj.Length()
	if err != nil {
		return WithStack(err)
	}

	if obj.IsSorted() {
		// object attributes are sorted
		previous, err := obj.KeyAt(0)
		if err != nil {
			return WithStack(err)
		}
		p, err := previous.GetString()
		if err != nil {
			return WithStack(err)
		}

		// compare each two adjacent attribute names
		for i := ValueLength(1); i < n; i++ {
			current, err := obj.KeyAt(i)
			if err != nil {
				return WithStack(err)
			}
			// keyAt() guarantees a string as returned type
			vpackAssert(current.IsString())

			q, err := current.GetString()
			if err != nil {
				return WithStack(err)
			}

			if p == q {
				// identical key
				return WithStack(DuplicateAttributeNameError)
			}
			// re-use already calculated values for next round
			p = q
		}
	} else {
		keys := make(map[string]struct{})

		for i := ValueLength(0); i < n; i++ {
			// note: keyAt() already translates integer attributes
			key, err := obj.KeyAt(i)
			if err != nil {
				return WithStack(err)
			}
			// keyAt() guarantees a string as returned type
			vpackAssert(key.IsString())

			k, err := key.GetString()
			if err != nil {
				return WithStack(err)
			}
			if _, found := keys[k]; found {
				return WithStack(DuplicateAttributeNameError)
			}
			keys[k] = struct{}{}
		}
	}
	return nil
}

func findAttrName(base []byte) ([]byte, error) {
	b := base[0]
	if b >= 0x40 && b <= 0xbe {
		// short UTF-8 string
		l := b - 0x40
		return base[1 : 1+l], nil
	}
	if b == 0xbf {
		// long UTF-8 string
		l := uint(0)
		// read string length
		for i := 8; i >= 1; i-- {
			l = (l << 8) + uint(base[i])
		}
		return base[1+8 : 1+8+l], nil
	}

	// translate attribute name
	key, err := Slice(base).makeKey()
	if err != nil {
		return nil, WithStack(err)
	}
	return findAttrName(key)
}

func (b *Builder) sortObjectIndex(objBase []byte, offsets []ValueLength) error {
	list := make(sortEntries, len(offsets))
	for i, off := range offsets {
		name, err := findAttrName(objBase[off:])
		if err != nil {
			return WithStack(err)
		}
		list[i] = sortEntry{
			Offset: off,
			Name:   name,
		}
	}
	list.Sort()
	//sort.Sort(list)
	for i, entry := range list {
		offsets[i] = entry.Offset
	}
	return nil
}

func (b *Builder) closeArray(tos ValueLength, index []ValueLength) {
	// fix head byte in case a compact Array was originally requested:
	b.buf[tos] = 0x06

	needIndexTable := true
	needNrSubs := true
	if len(index) == 1 {
		needIndexTable = false
		needNrSubs = false
	} else if (b.buf.Len()-tos)-index[0] == ValueLength(len(index))*(index[1]-index[0]) {
		// In this case it could be that all entries have the same length
		// and we do not need an offset table at all:
		noTable := true
		subLen := index[1] - index[0]
		if (b.buf.Len()-tos)-index[len(index)-1] != subLen {
			noTable = false
		} else {
			for i := 1; i < len(index)-1; i++ {
				if index[i+1]-index[i] != subLen {
					noTable = false
					break
				}
			}
		}
		if noTable {
			needIndexTable = false
			needNrSubs = false
		}
	}

	// First determine byte length and its format:
	var offsetSize uint
	// can be 1, 2, 4 or 8 for the byte width of the offsets,
	// the byte length and the number of subvalues:
	var indexLenIfNeeded ValueLength
	if needIndexTable {
		indexLenIfNeeded = ValueLength(len(index))
	}
	nrSubsLenIfNeeded := ValueLength(7)
	if needNrSubs {
		nrSubsLenIfNeeded = 6
	}
	if b.buf.Len()-tos+(indexLenIfNeeded)-(nrSubsLenIfNeeded) <= 0xff {
		// We have so far used _pos - tos bytes, including the reserved 8
		// bytes for byte length and number of subvalues. In the 1-byte number
		// case we would win back 6 bytes but would need one byte per subvalue
		// for the index table
		offsetSize = 1
	} else if b.buf.Len()-tos+(indexLenIfNeeded*2) <= 0xffff {
		offsetSize = 2
	} else if b.buf.Len()-tos+(indexLenIfNeeded*4) <= 0xffffffff {
		offsetSize = 4
	} else {
		offsetSize = 8
	}

	// Maybe we need to move down data:
	if offsetSize == 1 {
		targetPos := ValueLength(3)
		if !needIndexTable {
			targetPos = 2
		}
		if b.buf.Len() > (tos + 9) {
			_len := ValueLength(b.buf.Len() - (tos + 9))
			checkOverflow(_len)
			src := b.buf[tos+9:]
			copy(b.buf[tos+targetPos:], src[:_len])
		}
		diff := ValueLength(9 - targetPos)
		b.buf.Shrink(uint(diff))
		if needIndexTable {
			n := len(index)
			for i := 0; i < n; i++ {
				index[i] -= diff
			}
		} // Note: if !needIndexTable the index array is now wrong!
	}
	// One could move down things in the offsetSize == 2 case as well,
	// since we only need 4 bytes in the beginning. However, saving these
	// 4 bytes has been sacrificed on the Altar of Performance.

	// Now build the table:
	if needIndexTable {
		extraSpaceNeeded := offsetSize * uint(len(index))
		if offsetSize == 8 {
			extraSpaceNeeded += 8
		}
		b.buf.ReserveSpace(extraSpaceNeeded)
		tableBase := b.buf.Grow(offsetSize * uint(len(index)))
		for i := uint(0); i < uint(len(index)); i++ {
			x := uint64(index[i])
			for j := uint(0); j < offsetSize; j++ {
				tableBase[offsetSize*i+j] = byte(x & 0xff)
				x >>= 8
			}
		}
	} else { // no index table
		b.buf[tos] = 0x02
	}
	// Finally fix the byte width in the type byte:
	if offsetSize > 1 {
		if offsetSize == 2 {
			b.buf[tos] += 1
		} else if offsetSize == 4 {
			b.buf[tos] += 2
		} else { // offsetSize == 8
			b.buf[tos] += 3
			if needNrSubs {
				b.appendLength(ValueLength(len(index)), 8)
			}
		}
	}

	// Fix the byte length in the beginning:
	x := ValueLength(b.buf.Len() - tos)
	for i := uint(1); i <= offsetSize; i++ {
		b.buf[tos+ValueLength(i)] = byte(x & 0xff)
		x >>= 8
	}

	if offsetSize < 8 && needNrSubs {
		x = ValueLength(len(index))
		for i := offsetSize + 1; i <= 2*offsetSize; i++ {
			b.buf[tos+ValueLength(i)] = byte(x & 0xff)
			x >>= 8
		}
	}

	// Now the array or object is complete, we pop a ValueLength
	// off the _stack:
	b.stack.Pop()
	// Intentionally leave _index[depth] intact to avoid future allocs!
}

func (b *Builder) cleanupAdd() {
	depth := b.stack.Len() - 1
	b.index[depth].RemoveLast()
}

func (b *Builder) reportAdd() {
	tos, stackLen := b.stack.Tos()
	depth := stackLen - 1
	b.index[depth].Add(b.buf.Len() - tos)
}

func (b *Builder) addArray(unindexed ...bool) {
	h := byte(0x06)
	if optionalBool(unindexed, false) {
		h = 0x13
	}
	b.addCompoundValue(h)
}

func (b *Builder) addObject(unindexed ...bool) {
	h := byte(0x0b)
	if optionalBool(unindexed, false) {
		h = 0x14
	}
	b.addCompoundValue(h)
}

func (b *Builder) addInternal(v Value) error {
	haveReported := false
	if !b.stack.IsEmpty() {
		if !b.keyWritten {
			b.reportAdd()
			haveReported = true
		}
	}
	if err := b.set(v); err != nil {
		if haveReported {
			b.cleanupAdd()
		}
		return WithStack(err)
	}
	return nil
}

func (b *Builder) addInternalKeyValue(attrName string, v Value) error {
	haveReported, err := b.addInternalKey(attrName)
	if err != nil {
		return WithStack(err)
	}
	if err := b.set(v); err != nil {
		if haveReported {
			b.cleanupAdd()
		}
		return WithStack(err)
	}
	return nil
}

func (b *Builder) addInternalKey(attrName string) (haveReported bool, err error) {
	haveReported = false
	tos, stackLen := b.stack.Tos()
	if stackLen > 0 {
		h := b.buf[tos]
		if h != 0x0b && h != 0x14 {
			return haveReported, WithStack(BuilderNeedOpenObjectError)
		}
		if b.keyWritten {
			return haveReported, WithStack(BuilderKeyAlreadyWrittenError)
		}
		b.reportAdd()
		haveReported = true
	}

	onError := func() {
		if haveReported {
			b.cleanupAdd()
			haveReported = false
		}
	}

	if err := b.set(NewStringValue(attrName)); err != nil {
		onError()
		return haveReported, WithStack(err)
	}
	b.keyWritten = true
	return haveReported, nil
}

func (b *Builder) checkKeyIsString(isString bool) error {
	tos, stackLen := b.stack.Tos()
	if stackLen > 0 {
		h := b.buf[tos]
		if h == 0x0b || h == 0x14 {
			if !b.keyWritten {
				if isString {
					b.keyWritten = true
				} else {
					return WithStack(BuilderKeyMustBeStringError)
				}
			} else {
				b.keyWritten = false
			}
		}
	}
	return nil
}

func (b *Builder) set(item Value) error {
	//oldPos := b.buf.Len()
	//ctype := item.vt

	if err := b.checkKeyIsString(item.vt == String); err != nil {
		return WithStack(err)
	}

	if item.IsSlice() {
		switch item.vt {
		case None:
			return WithStack(BuilderUnexpectedTypeError{"Cannot set a ValueType::None"})
		case External:
			return fmt.Errorf("External not supported")
		case Custom:
			return WithStack(fmt.Errorf("Cannot set a ValueType::Custom with this method"))
		}
		s := item.sliceValue()
		// Determine length of slice
		l, err := s.ByteSize()
		if err != nil {
			return WithStack(err)
		}
		b.buf.Write(s[:l])
		return nil
	}

	// This method builds a single further VPack item at the current
	// append position. If this is an array or object, then an index
	// table is created and a new ValueLength is pushed onto the stack.
	switch item.vt {
	case None:
		return WithStack(BuilderUnexpectedTypeError{"Cannot set a ValueType::None"})
	case Null:
		b.addNull()
	case Bool:
		b.addBool(item.boolValue())
	case Double:
		b.addDouble(item.doubleValue())
	case External:
		return fmt.Errorf("External not supported")
		/*if (options->disallowExternals) {
		    // External values explicitly disallowed as a security
		    // precaution
		    throw Exception(Exception::BuilderExternalsDisallowed);
		  }
		  if (ctype != Value::CType::VoidPtr) {
		    throw Exception(Exception::BuilderUnexpectedValue,
		                    "Must give void pointer for ValueType::External");
		  }
		  reserveSpace(1 + sizeof(void*));
		  // store pointer. this doesn't need to be portable
		  _start[_pos++] = 0x1d;
		  void const* value = item.getExternal();
		  memcpy(_start + _pos, &value, sizeof(void*));
		  _pos += sizeof(void*);
		  break;
		}*/
	case SmallInt:
		b.addInt(item.intValue())
	case Int:
		b.addInt(item.intValue())
	case UInt:
		b.addUInt(item.uintValue())
	case UTCDate:
		b.addUTCDate(item.utcDateValue())
	case String:
		b.addString(item.stringValue())
	case Array:
		b.addArray(item.unindexed)
	case Object:
		b.addObject(item.unindexed)
	case Binary:
		b.addBinary(item.binaryValue())
	case Illegal:
		b.addIllegal()
	case MinKey:
		b.addMinKey()
	case MaxKey:
		b.addMaxKey()
	case BCD:
		return WithStack(fmt.Errorf("Not implemented"))
	case Custom:
		return WithStack(fmt.Errorf("Cannot set a ValueType::Custom with this method"))
	}
	return nil
}
