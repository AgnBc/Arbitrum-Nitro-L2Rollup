use arbutil::PreimageType;
use prover::machine::{argument_data_to_inbox, GlobalState, Machine};
use prover::utils::{Bytes32, CBytes};
use std::fs::File;
use std::io::BufReader;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use crate::parse_nova_data::*;

pub fn prepare_machine(info_data: PathBuf, machines: PathBuf) -> eyre::Result<Machine> {
    let file = File::open(&info_data)?;
    let reader = BufReader::new(file);

    let data = FileData::from_reader(reader)?;
    // let item = data.items.get(0).unwrap().clone();
    // let preimages = item.preimages;
    // let preimages = preimages
    //     .into_iter()
    //     .map(|preimage| {
    //         let hash: [u8; 32] = preimage.hash.try_into().unwrap();
    //         let hash: Bytes32 = hash.into();
    //         (hash, preimage.data)
    //     })
    //     .collect::<HashMap<Bytes32, Vec<u8>>>();
    let preimage_resolver = move |_: u64, _: PreimageType, _hash: Bytes32| -> Option<CBytes> {
        // preimages
        //     .get(&hash)
        //     .map(|data| CBytes::from(data.as_slice()))
        None
    };
    let preimage_resolver = Arc::new(Box::new(preimage_resolver));

    let binary_path = Path::new(&machines);

    let mut mach = Machine::new_from_wavm(binary_path)?;

    let block_hash: [u8; 32] = data.start_state.block_hash.try_into().unwrap();
    let block_hash: Bytes32 = block_hash.into();
    let send_root: [u8; 32] = data.start_state.send_root.try_into().unwrap();
    let send_root: Bytes32 = send_root.into();
    let bytes32_vals: [Bytes32; 2] = [block_hash, send_root];
    let u64_vals: [u64; 2] = [data.start_state.batch, data.start_state.pos_in_batch];
    let start_state = GlobalState {
        bytes32_vals,
        u64_vals,
    };

    mach.set_global_state(start_state);
    mach.set_preimage_resolver(preimage_resolver);
    let identifier = argument_data_to_inbox(0).unwrap();
    mach.add_inbox_msg(identifier, data.pos, data.msg);
    let identifier = argument_data_to_inbox(1).unwrap();
    mach.add_inbox_msg(identifier, data.delayed_msg_nr, data.delayed_msg);

    Ok(mach)
}
