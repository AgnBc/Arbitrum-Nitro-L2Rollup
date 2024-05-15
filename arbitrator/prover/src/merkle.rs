// Copyright 2021-2023, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

use arbutil::Bytes32;
use digest::Digest;

use enum_iterator::Sequence;

#[cfg(feature = "counters")]
use enum_iterator::all;
use itertools::Itertools;

#[cfg(feature = "counters")]
use std::sync::atomic::AtomicUsize;

#[cfg(feature = "counters")]
use std::sync::atomic::Ordering;

#[cfg(feature = "counters")]
use lazy_static::lazy_static;

#[cfg(feature = "counters")]
use std::collections::HashMap;

use core::panic;
use serde::{Deserialize, Serialize};
use sha3::Keccak256;
use std::{
    collections::HashSet,
    convert::TryInto,
    sync::{Arc, Mutex},
};

mod zerohashes;

use zerohashes::ZERO_HASHES;

use self::zerohashes::EMPTY_HASH;

#[cfg(feature = "counters")]
lazy_static! {
    static ref NEW_COUNTERS: HashMap<&'static MerkleType, AtomicUsize> = {
        let mut map = HashMap::new();
        map.insert(&MerkleType::Empty, AtomicUsize::new(0));
        map.insert(&MerkleType::Value, AtomicUsize::new(0));
        map.insert(&MerkleType::Function, AtomicUsize::new(0));
        map.insert(&MerkleType::Instruction, AtomicUsize::new(0));
        map.insert(&MerkleType::Memory, AtomicUsize::new(0));
        map.insert(&MerkleType::Table, AtomicUsize::new(0));
        map.insert(&MerkleType::TableElement, AtomicUsize::new(0));
        map.insert(&MerkleType::Module, AtomicUsize::new(0));
        map
    };
}
#[cfg(feature = "counters")]
lazy_static! {
    static ref ROOT_COUNTERS: HashMap<&'static MerkleType, AtomicUsize> = {
        let mut map = HashMap::new();
        map.insert(&MerkleType::Empty, AtomicUsize::new(0));
        map.insert(&MerkleType::Value, AtomicUsize::new(0));
        map.insert(&MerkleType::Function, AtomicUsize::new(0));
        map.insert(&MerkleType::Instruction, AtomicUsize::new(0));
        map.insert(&MerkleType::Memory, AtomicUsize::new(0));
        map.insert(&MerkleType::Table, AtomicUsize::new(0));
        map.insert(&MerkleType::TableElement, AtomicUsize::new(0));
        map.insert(&MerkleType::Module, AtomicUsize::new(0));
        map
    };
}
#[cfg(feature = "counters")]
lazy_static! {
    static ref SET_COUNTERS: HashMap<&'static MerkleType, AtomicUsize> = {
        let mut map = HashMap::new();
        map.insert(&MerkleType::Empty, AtomicUsize::new(0));
        map.insert(&MerkleType::Value, AtomicUsize::new(0));
        map.insert(&MerkleType::Function, AtomicUsize::new(0));
        map.insert(&MerkleType::Instruction, AtomicUsize::new(0));
        map.insert(&MerkleType::Memory, AtomicUsize::new(0));
        map.insert(&MerkleType::Table, AtomicUsize::new(0));
        map.insert(&MerkleType::TableElement, AtomicUsize::new(0));
        map.insert(&MerkleType::Module, AtomicUsize::new(0));
        map
    };
}
#[cfg(feature = "counters")]
lazy_static! {
    static ref RESIZE_COUNTERS: HashMap<&'static MerkleType, AtomicUsize> = {
        let mut map = HashMap::new();
        map.insert(&MerkleType::Empty, AtomicUsize::new(0));
        map.insert(&MerkleType::Value, AtomicUsize::new(0));
        map.insert(&MerkleType::Function, AtomicUsize::new(0));
        map.insert(&MerkleType::Instruction, AtomicUsize::new(0));
        map.insert(&MerkleType::Memory, AtomicUsize::new(0));
        map.insert(&MerkleType::Table, AtomicUsize::new(0));
        map.insert(&MerkleType::TableElement, AtomicUsize::new(0));
        map.insert(&MerkleType::Module, AtomicUsize::new(0));
        map
    };
}

#[derive(Debug, Clone, Copy, Hash, PartialEq, Eq, Serialize, Deserialize, Sequence)]
pub enum MerkleType {
    Empty,
    Value,
    Function,
    Instruction,
    Memory,
    Table,
    TableElement,
    Module,
}

impl Default for MerkleType {
    fn default() -> Self {
        Self::Empty
    }
}

#[cfg(feature = "counters")]
pub fn print_counters() {
    for ty in all::<MerkleType>() {
        if ty == MerkleType::Empty {
            continue;
        }
        println!(
            "{} New: {}, Root: {}, Set: {} Resize: {}",
            ty.get_prefix(),
            NEW_COUNTERS[&ty].load(Ordering::Relaxed),
            ROOT_COUNTERS[&ty].load(Ordering::Relaxed),
            SET_COUNTERS[&ty].load(Ordering::Relaxed),
            RESIZE_COUNTERS[&ty].load(Ordering::Relaxed),
        );
    }
}

#[cfg(feature = "counters")]
pub fn reset_counters() {
    for ty in all::<MerkleType>() {
        if ty == MerkleType::Empty {
            continue;
        }
        NEW_COUNTERS[&ty].store(0, Ordering::Relaxed);
        ROOT_COUNTERS[&ty].store(0, Ordering::Relaxed);
        SET_COUNTERS[&ty].store(0, Ordering::Relaxed);
        RESIZE_COUNTERS[&ty].store(0, Ordering::Relaxed);
    }
}

impl MerkleType {
    pub fn get_prefix(self) -> &'static str {
        match self {
            MerkleType::Empty => panic!("Attempted to get prefix of empty merkle type"),
            MerkleType::Value => "Value merkle tree:",
            MerkleType::Function => "Function merkle tree:",
            MerkleType::Instruction => "Instruction merkle tree:",
            MerkleType::Memory => "Memory merkle tree:",
            MerkleType::Table => "Table merkle tree:",
            MerkleType::TableElement => "Table element merkle tree:",
            MerkleType::Module => "Module merkle tree:",
        }
    }
}

/// A Merkle tree with a fixed number of layers
///
/// https://en.wikipedia.org/wiki/Merkle_tree
///
/// Each instance's leaves contain the hashes of a specific [MerkleType].
/// The tree does not grow in height, but it can be initialized with fewer
/// leaves than the number that could be contained in its layers.
///
/// When initialized with [Merkle::new], the tree has the minimum depth
/// necessary to hold all the leaves. (e.g. 5 leaves -> 4 layers.)
///
/// It can be over-provisioned using the [Merkle::new_advanced] method
/// and passing a minimum depth.
///
/// This structure does not contain the data itself, only the hashes.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Merkle {
    ty: MerkleType,
    #[serde(with = "arc_mutex_sedre")]
    tree: Arc<Mutex<Vec<u8>>>,
    depth: usize,
    layer0_len: Arc<Mutex<usize>>,
    #[serde(with = "arc_mutex_sedre")]
    dirty_layers: Arc<Mutex<Vec<HashSet<usize>>>>,
}

fn hash_node(ty: MerkleType, a: impl AsRef<[u8]>, b: impl AsRef<[u8]>) -> Bytes32 {
    let mut h = Keccak256::new();
    h.update(ty.get_prefix());
    h.update(a);
    h.update(b);
    h.finalize().into()
}

const fn empty_hash_at(ty: MerkleType, layer_i: usize) -> &'static Bytes32 {
    match ty {
        MerkleType::Empty => EMPTY_HASH,
        MerkleType::Value => &ZERO_HASHES[0][layer_i],
        MerkleType::Function => &ZERO_HASHES[1][layer_i],
        MerkleType::Instruction => &ZERO_HASHES[2][layer_i],
        MerkleType::Memory => &ZERO_HASHES[3][layer_i],
        MerkleType::Table => &ZERO_HASHES[4][layer_i],
        MerkleType::TableElement => &ZERO_HASHES[5][layer_i],
        MerkleType::Module => &ZERO_HASHES[6][layer_i],
    }
}

impl Merkle {
    /// Creates a new Merkle tree with the given type and leaf hashes.
    /// The tree is built up to the minimum depth necessary to hold all the
    /// leaves.
    pub fn new(ty: MerkleType, hashes: Vec<Bytes32>) -> Merkle {
        Self::new_advanced(ty, hashes, 0)
    }

    /// Creates a new Merkle tree with the given type, leaf hashes, a hash to
    /// use for representing empty leaves, and a minimum depth.
    pub fn new_advanced(ty: MerkleType, hashes: Vec<Bytes32>, min_depth: usize) -> Merkle {
        #[cfg(feature = "counters")]
        NEW_COUNTERS[&ty].fetch_add(1, Ordering::Relaxed);
        if hashes.is_empty() && min_depth == 0 {
            return Merkle::default();
        }

        let hash_count = hashes.len();
        let mut target_depth = (hash_count as f64).log2().ceil() as usize;
        target_depth = target_depth + 1;
        target_depth = target_depth.max(min_depth);

        // Calculate the total capacity needed for the tree
        let total_capacity = calculate_total_capacity(target_depth, hash_count);

        let mut tree = Vec::with_capacity(total_capacity);

        // Append initial hashes to the tree
        for hash in hashes.into_iter() {
            tree.extend_from_slice(hash.as_slice());
        }

        let mut current_level_size = hash_count;
        let mut next_level_offset = tree.len();
        let mut depth = target_depth;

        let mut dirty_indices: Vec<HashSet<usize>> = Vec::with_capacity(depth);
        let mut layer_i = 0usize;
        while depth > 1 {
            let mut i = next_level_offset - current_level_size * 32;
            while i < next_level_offset {
                let left = &tree[i..i + 32];
                let right = if i + 32 < next_level_offset {
                    &tree[i + 32..i + 64]
                } else {
                    empty_hash_at(ty, layer_i).as_slice()
                };

                let parent_hash = hash_node(ty, left, right);
                tree.extend(parent_hash.as_slice());

                i += 64;
            }

            current_level_size = (current_level_size + 1) / 2;
            dirty_indices.push(HashSet::with_capacity(current_level_size));
            next_level_offset = tree.len();
            depth = depth.saturating_sub(1);
            layer_i += 1;
        }
        let dirty_layers = Arc::new(Mutex::new(dirty_indices));
        Merkle {
            ty,
            tree: Arc::new(Mutex::new(tree)),
            depth: target_depth,
            layer0_len: Arc::new(Mutex::new(hash_count)),
            dirty_layers,
        }
    }

    #[inline(never)]
    fn rehash(&self) {
        let dirty_layers = &mut self.dirty_layers.lock().unwrap();
        if dirty_layers.is_empty() || dirty_layers[0].is_empty() {
            return;
        }
        let mut tree = self.tree.lock().unwrap();
        let mut child_layer_start = 0usize;
        let mut layer_start = self.calculate_layer_size(0) * 32;
        let mut layer_size = self.calculate_layer_size(1) * 32;
        for layer_i in 1..self.depth {
            let dirty_i = layer_i - 1;
            let dirt = dirty_layers[dirty_i].clone();
            for idx in dirt.iter().sorted() {
                let child_layer_size = self.calculate_layer_size(layer_i - 1) * 32;
                let left_child_idx = idx << 1;
                let right_child_idx = left_child_idx + 1;
                let left = get_node(&tree, child_layer_start, left_child_idx);
                let right = if child_layer_start + right_child_idx * 32
                    < child_layer_start + child_layer_size
                {
                    get_node(&tree, child_layer_start, right_child_idx)
                } else {
                    *empty_hash_at(self.ty, layer_i - 1)
                };
                let new_hash = hash_node(self.ty, left, right);
                let layer_idx = layer_start + idx * 32;
                if layer_idx < layer_start + layer_size {
                    tree[layer_idx..layer_idx + 32].copy_from_slice(new_hash.as_slice());
                } else {
                    panic!(
                        "Index out of bounds: {} >= {}",
                        layer_idx,
                        layer_start + layer_size
                    );
                }
                if layer_i < self.depth - 1 {
                    dirty_layers[dirty_i + 1].insert(idx >> 1);
                }
            }
            (child_layer_start, layer_start) = (layer_start, layer_start + layer_size);
            layer_size = self.calculate_layer_size(layer_i + 1) * 32;
            dirty_layers[dirty_i].clear();
        }
    }

    pub fn root(&self) -> Bytes32 {
        #[cfg(feature = "counters")]
        ROOT_COUNTERS[&self.ty].fetch_add(1, Ordering::Relaxed);
        if self.is_empty() {
            return *empty_hash_at(self.ty, 0);
        }
        self.rehash();
        let tree = self.tree.lock().unwrap();
        let len = tree.len();
        let mut root = [0u8; 32];
        root.copy_from_slice(&tree[len - 32..len]);
        root.into()
    }

    // Returns the total number of leaves the tree can hold.
    #[inline]
    fn capacity(&self) -> usize {
        let tree = self.tree.lock().unwrap();
        if tree.is_empty() && self.depth == 0 {
            return 0;
        }
        let base: usize = 2;
        base.pow((self.depth - 1).try_into().unwrap())
    }

    // Returns the number of leaves in the tree.
    pub fn len(&self) -> usize {
        self.calculate_layer_size(0)
    }

    pub fn is_empty(&self) -> bool {
        self.tree.lock().unwrap().is_empty()
    }

    #[must_use]
    pub fn prove(&self, idx: usize) -> Option<Vec<u8>> {
        if self.is_empty() || idx >= self.len() {
            return None;
        }
        Some(self.prove_any(idx))
    }

    /// creates a merkle proof regardless of if the leaf has content
    #[must_use]
    pub fn prove_any(&self, idx: usize) -> Vec<u8> {
        self.rehash();

        let mut proof = Vec::with_capacity(self.depth * 32);
        let mut node_index = idx;
        let mut layer_start = 0;

        for depth in 0.. {
            let layer_size = self.calculate_layer_size(depth);
            if layer_size == 0 {
                break;
            }

            let sibling_index = if node_index % 2 == 0 {
                node_index + 1
            } else {
                node_index - 1
            };
            if sibling_index < layer_size {
                proof.extend(get_node(
                    &self.tree.lock().unwrap(),
                    layer_start,
                    sibling_index,
                ));
            } else {
                proof.extend(*empty_hash_at(self.ty, depth));
            }

            node_index >>= 1;
            layer_start += layer_size * 32;
        }
        proof
    }

    /// Adds a new leaf to the merkle
    /// Currently O(n) in the number of leaves (could be log(n))
    pub fn push_leaf(&mut self, leaf: Bytes32) {
        let mut leaves = self.leaves();
        leaves.push(leaf);
        *self = Self::new_advanced(self.ty, leaves, self.depth);
    }

    /// Removes the rightmost leaf from the merkle
    /// Currently O(n) in the number of leaves (could be log(n))
    pub fn pop_leaf(&mut self) {
        let mut leaves = self.leaves();
        leaves.pop();
        *self = Self::new_advanced(self.ty, leaves, self.depth);
    }

    // Sets the leaf at the given index to the given hash.
    // Panics if the index is out of bounds (since the structure doesn't grow).
    pub fn set(&self, idx: usize, hash: Bytes32) {
        #[cfg(feature = "counters")]
        SET_COUNTERS[&self.ty].fetch_add(1, Ordering::Relaxed);
        if idx >= self.len() {
            panic!("index {} out of bounds {}", idx, self.len());
        }
        let mut tree = self.tree.lock().unwrap();
        if tree[idx * 32..idx * 32 + 32].eq(hash.as_slice()) {
            return;
        }
        tree[idx * 32..idx * 32 + 32].copy_from_slice(hash.as_slice());
        self.dirty_layers.lock().unwrap()[0].insert(idx >> 1);
    }

    /// Resizes the number of leaves the tree can hold.
    ///
    /// The extra space is filled with empty hashes.
    pub fn resize(&self, new_len: usize) -> Result<usize, String> {
        #[cfg(feature = "counters")]
        RESIZE_COUNTERS[&self.ty].fetch_add(1, Ordering::Relaxed);
        if new_len > self.capacity() {
            return Err(format!(
                "Cannot resize to a length ({}) greater than the capacity ({})) of the tree.",
                new_len,
                self.capacity()
            ));
        }

        let mut new_tree = Vec::with_capacity(calculate_total_capacity(self.depth, new_len));
        let mut tree = self.tree.lock().unwrap();
        let mut layer_offset = 0;
        let mut new_next_layer_offset = new_len * 32;
        for layer_i in 0..self.depth {
            new_tree.extend_from_slice(
                &tree[layer_offset..(layer_offset + self.calculate_layer_size(layer_i) * 32)],
            );
            while new_tree.len() < new_next_layer_offset {
                new_tree.extend_from_slice(empty_hash_at(self.ty, layer_i).as_slice());
            }
            layer_offset += self.calculate_layer_size(layer_i) * 32;
            new_next_layer_offset =
                new_tree.len() + calculate_layer_size(self.depth, new_len, layer_i + 1) * 32;
        }
        let start = self.len();
        for i in start..new_len {
            self.dirty_layers.lock().unwrap()[0].insert(i >> 1);
        }
        *tree = new_tree;
        *self.layer0_len.lock().unwrap() = new_len;
        Ok(self.len())
    }

    // Helper function to get the leaves of the tree
    #[inline(always)]
    fn leaves(&self) -> Vec<Bytes32> {
        let tree = self.tree.lock().unwrap();
        let mut leaves = Vec::with_capacity(*self.layer0_len.lock().unwrap());
        for i in 0..*self.layer0_len.lock().unwrap() {
            let start = i * 32;
            let mut leaf = [0u8; 32];
            leaf.copy_from_slice(&tree[start..start + 32]);
            leaves.push(leaf.into());
        }
        leaves
    }

    // Helper function to calculate the size of a given layer
    #[inline(always)]
    fn calculate_layer_size(&self, layer: usize) -> usize {
        calculate_layer_size(self.depth, *self.layer0_len.lock().unwrap(), layer)
    }
}

// Helper function to get a node from the tree
#[inline(always)]
fn get_node(tree: &Vec<u8>, layer_start: usize, index: usize) -> Bytes32 {
    let start = layer_start + index * 32;
    let mut node = [0u8; 32];
    node.copy_from_slice(&tree[start..start + 32]);
    node.into()
}

fn calculate_layer_size(depth: usize, layer0_len: usize, layer: usize) -> usize {
    if layer >= depth {
        return 0;
    }
    let mut size = layer0_len;
    for _ in 0..layer {
        size = (size + 1) / 2;
    }
    size
}

fn calculate_total_capacity(depth: usize, layer0_len: usize) -> usize {
    let mut total_capacity = layer0_len * 32;
    let mut current_level_size = layer0_len;
    let mut depth = depth;
    while depth > 1 {
        current_level_size = (current_level_size + 1) / 2;
        total_capacity += current_level_size * 32;
        depth = depth.saturating_sub(1);
    }
    total_capacity
}

impl PartialEq for Merkle {
    fn eq(&self, other: &Self) -> bool {
        self.root() == other.root()
    }
}

impl Eq for Merkle {}

pub mod arc_mutex_sedre {
    pub fn serialize<S, T>(
        data: &std::sync::Arc<std::sync::Mutex<T>>,
        serializer: S,
    ) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
        T: serde::Serialize,
    {
        data.lock().unwrap().serialize(serializer)
    }

    pub fn deserialize<'de, D, T>(
        deserializer: D,
    ) -> Result<std::sync::Arc<std::sync::Mutex<T>>, D::Error>
    where
        D: serde::Deserializer<'de>,
        T: serde::Deserialize<'de>,
    {
        Ok(std::sync::Arc::new(std::sync::Mutex::new(T::deserialize(
            deserializer,
        )?)))
    }
}

#[test]
fn resize_works() {
    let hashes = vec![
        Bytes32::from([1; 32]),
        Bytes32::from([2; 32]),
        Bytes32::from([3; 32]),
        Bytes32::from([4; 32]),
        Bytes32::from([5; 32]),
    ];
    let mut expected = hash_node(
        MerkleType::Value,
        hash_node(
            MerkleType::Value,
            hash_node(
                MerkleType::Value,
                Bytes32::from([1; 32]),
                Bytes32::from([2; 32]),
            ),
            hash_node(
                MerkleType::Value,
                Bytes32::from([3; 32]),
                Bytes32::from([4; 32]),
            ),
        ),
        hash_node(
            MerkleType::Value,
            hash_node(
                MerkleType::Value,
                Bytes32::from([5; 32]),
                Bytes32::from([0; 32]),
            ),
            hash_node(
                MerkleType::Value,
                Bytes32::from([0; 32]),
                Bytes32::from([0; 32]),
            ),
        ),
    );
    let merkle = Merkle::new(MerkleType::Value, hashes.clone());
    assert_eq!(merkle.capacity(), 8);
    assert_eq!(merkle.root(), expected);

    let new_size = match merkle.resize(6) {
        Ok(size) => size,
        Err(e) => panic!("{}", e),
    };
    assert_eq!(new_size, 6);
    merkle.set(5, Bytes32::from([6; 32]));
    expected = hash_node(
        MerkleType::Value,
        hash_node(
            MerkleType::Value,
            hash_node(
                MerkleType::Value,
                Bytes32::from([1; 32]),
                Bytes32::from([2; 32]),
            ),
            hash_node(
                MerkleType::Value,
                Bytes32::from([3; 32]),
                Bytes32::from([4; 32]),
            ),
        ),
        hash_node(
            MerkleType::Value,
            hash_node(
                MerkleType::Value,
                Bytes32::from([5; 32]),
                Bytes32::from([6; 32]),
            ),
            hash_node(
                MerkleType::Value,
                Bytes32::from([0; 32]),
                Bytes32::from([0; 32]),
            ),
        ),
    );
    assert_eq!(merkle.root(), expected);
}

#[test]
fn correct_capacity() {
    let merkle = Merkle::new(MerkleType::Value, vec![Bytes32::from([1; 32])]);
    assert_eq!(merkle.capacity(), 1);
    let merkle = Merkle::new_advanced(MerkleType::Memory, vec![Bytes32::from([1; 32])], 11);
    assert_eq!(merkle.capacity(), 1024);
}

#[test]
#[ignore = "This is just used for generating the zero hashes for the memory merkle trees."]
fn emit_memory_zerohashes() {
    // The following code was generated from the empty_leaf_hash() test in the memory package.
    let mut empty_node = Bytes32::new_direct([
        57, 29, 211, 154, 252, 227, 18, 99, 65, 126, 203, 166, 252, 232, 32, 3, 98, 194, 254, 186,
        118, 14, 139, 192, 101, 156, 55, 194, 101, 11, 11, 168,
    ])
    .clone();
    for _ in 0..64 {
        print!("Bytes32::new_direct([");
        for i in 0..32 {
            print!("{}", empty_node[i]);
            if i < 31 {
                print!(", ");
            }
        }
        println!("]),");
        empty_node = hash_node(MerkleType::Memory, empty_node, empty_node);
    }
}

#[test]
fn calculate_layer_sizes() {
    let expect = 128usize;
    let actual = calculate_layer_size(11, 1024, 3);
    assert_eq!(expect, actual);

    let expect = 1usize;
    let actual = calculate_layer_size(11, 1024, 10);
    assert_eq!(expect, actual);

    let expect = 3usize;
    let actual = calculate_layer_size(4, 6, 1);
    assert_eq!(expect, actual);

    let expect = 3usize;
    let actual = calculate_layer_size(4, 5, 1);
    assert_eq!(expect, actual);

    let expect = 5usize;
    let actual = calculate_layer_size(4, 5, 0);
    assert_eq!(expect, actual);

    let expect = 2usize;
    let actual = calculate_layer_size(4, 5, 2);
    assert_eq!(expect, actual);

    let expect = 2usize;
    let actual = calculate_layer_size(4, 4, 1);
    assert_eq!(expect, actual);
}

#[test]
fn serialization_roundtrip() {
    let merkle = Merkle::new_advanced(MerkleType::Value, vec![Bytes32::from([1; 32])], 4);
    merkle.resize(4).expect("resize failed");
    merkle.set(3, Bytes32::from([2; 32]));
    let serialized = bincode::serialize(&merkle).unwrap();
    let deserialized: Merkle = bincode::deserialize(&serialized).unwrap();
    assert_eq!(merkle, deserialized);
}

#[test]
#[should_panic(expected = "index 2 out of bounds 2")]
fn set_with_bad_index_panics() {
    let merkle = Merkle::new(
        MerkleType::Value,
        vec![Bytes32::default(), Bytes32::default()],
    );
    assert_eq!(merkle.capacity(), 2);
    merkle.set(2, Bytes32::default());
}
