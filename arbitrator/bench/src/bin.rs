use std::{
    path::PathBuf,
    time::{Duration, Instant},
};

use bench::prepare::*;
use clap::Parser;
use eyre::bail;
use prover::{
    flat_merkle,
    machine::MachineStatus,
    merkle::{Merkle, MerkleType},
    utils::Bytes32,
};

#[derive(Parser, Debug)]
#[command(author, version, about, long_about = None)]
struct Args {
    /// Path to a preimages text file
    #[arg(short, long)]
    preimages_path: PathBuf,

    /// Path to a machine.wavm.br
    #[arg(short, long)]
    machine_path: PathBuf,
}

fn main() -> eyre::Result<()> {
    // benchmark_merkle()
    benchmark_machines()
}

const MEMORY_LAYERS: usize = 28;

fn benchmark_merkle() -> eyre::Result<()> {
    let mut hashes = vec![];
    for i in 0..10_000 {
        hashes.push(Bytes32::from(i as u64));
    }
    let start = Instant::now();
    let tr = Merkle::new_advanced(
        MerkleType::Memory,
        hashes,
        Bytes32::default(),
        MEMORY_LAYERS,
    );
    println!(
        "Time with normal merkle: {:?}, root {:?}",
        start.elapsed(),
        hex::encode(tr.root())
    );
    let mut hashes = vec![];
    for i in 0..10_000 {
        hashes.push(Bytes32::from(i as u64));
    }
    let start = Instant::now();
    let tr = flat_merkle::Merkle::new_advanced(
        flat_merkle::MerkleType::Memory,
        hashes,
        Bytes32::default(),
        MEMORY_LAYERS,
    );
    println!(
        "Time with flat merkle: {:?}, got root {:?}",
        start.elapsed(),
        hex::encode(tr.root()),
    );
    Ok(())
}

fn benchmark_machines() -> eyre::Result<()> {
    let args = Args::parse();
    let step_sizes = [1 << 20];
    for step_size in step_sizes {
        let mut machine = prepare_machine(args.preimages_path.clone(), args.machine_path.clone())?;
        let _ = machine.hash();
        let mut hash_times = vec![];
        let mut step_times = vec![];
        let mut num_iters = 0;
        loop {
            let start = std::time::Instant::now();
            machine.step_n(step_size)?;
            let step_end_time = start.elapsed();
            step_times.push(step_end_time);
            match machine.get_status() {
                MachineStatus::Errored => {
                    println!("Errored");
                    break;
                    // bail!("Machine errored => position {}", machine.get_steps())
                }
                MachineStatus::TooFar => {
                    bail!("Machine too far => position {}", machine.get_steps())
                }
                MachineStatus::Running => {}
                MachineStatus::Finished => return Ok(()),
            }
            let start = std::time::Instant::now();
            let _ = machine.hash();
            let hash_end_time = start.elapsed();
            hash_times.push(hash_end_time);
            num_iters += 1;
            if num_iters == 16384 * 2 {
                break;
            }
        }
        println!(
            "avg hash time {:?}, avg step time {:?}, step size {}, num_iters {}",
            average(&hash_times),
            average(&step_times),
            step_size,
            num_iters,
        );
    }
    Ok(())
}

fn average(numbers: &[Duration]) -> Duration {
    let sum: Duration = numbers.iter().sum();
    let sum: u64 = sum.as_nanos().try_into().unwrap();
    Duration::from_nanos(sum / numbers.len() as u64)
}
